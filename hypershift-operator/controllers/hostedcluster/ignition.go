/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hostedcluster

import (
	"fmt"

	ignitionserverv2 "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver"
	ignitionproxyv2 "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver_proxy"
	controlplanecomponent "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"
)

// reconcileIgnitionPayloadComponents deploys the split ignition system (IgnitionPayloadSystem feature
// gate) from the HyperShift Operator image: the leader-elected generator, the stateless serving tier,
// and the reused node-facing proxy. It mirrors reconcileKarpenterOperator — the HO reconciles these
// ControlPlaneComponents directly rather than the CPO, which stands down the same-named components when
// the gate is active (feature-set mutual exclusion). The component name, Service, Route, serving PKI and
// node-facing CA are all reused, so the node endpoint and trust do not change across the cutover.
func (r *HostedClusterReconciler) reconcileIgnitionPayloadComponents(cpContext controlplanecomponent.ControlPlaneContext, hypershiftOperatorImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides) error {
	defaultIngressDomain, err := r.defaultIngressDomain(cpContext)
	if err != nil {
		return fmt.Errorf("failed to determine default ingress domain: %w", err)
	}

	// Serving half: the stateless server that verifies node tokens and serves from the PayloadStore.
	// Reuses the ignition-server component name/Service/Route/serving-cert; runs the HO image's
	// ignition-payload-server subcommand.
	//
	// This MUST be reconciled before the generator: the serving component authors the ignition-server
	// Route, and the generator's adapt resolves that Route's admitted host to bake the node endpoint
	// into the userData it generates — returning an error ("ignition server address not yet available")
	// until the host is known. Reconciling the generator first would return that error before the
	// serving component ever creates the Route, so the Route would never be created and the generator
	// could never resolve it: a permanent deadlock. The serving component's Reconcile returns nil even
	// while its pod waits on the generator-authored signing-key secret (pod readiness is a status
	// condition, not a Reconcile error), so ordering it first does not block the generator.
	server := ignitionserverv2.NewServerComponent(hypershiftOperatorImage, defaultIngressDomain)
	if err := server.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server component: %w", err)
	}

	// Generation half: the leader-elected controller that authors payloads into the PayloadStore and
	// mints node bearer tokens. Runs the HO image's ignition-payload-controller subcommand. Reconciled
	// after the serving component so the ignition-server Route it depends on already exists.
	generator := ignitionserverv2.NewGeneratorComponent(hypershiftOperatorImage, releaseProvider)
	if err := generator.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server-generator component: %w", err)
	}

	// Node-facing proxy (reused unchanged); its predicate skips IBM Cloud, where the server is exposed
	// directly.
	proxy := ignitionproxyv2.NewComponent(defaultIngressDomain)
	if err := proxy.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server-proxy component: %w", err)
	}

	return nil
}
