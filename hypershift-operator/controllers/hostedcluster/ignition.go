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

	// Generation half: the leader-elected controller that authors payloads into the PayloadStore and
	// mints node bearer tokens. Runs the HO image's ignition-payload-controller subcommand.
	generator := ignitionserverv2.NewGeneratorComponent(hypershiftOperatorImage, releaseProvider)
	if err := generator.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server-generator component: %w", err)
	}

	// Serving half: the stateless server that verifies node tokens and serves from the PayloadStore.
	// Reuses the ignition-server component name/Service/Route/serving-cert; runs the HO image's
	// ignition-payload-server subcommand.
	server := ignitionserverv2.NewServerComponent(hypershiftOperatorImage, defaultIngressDomain)
	if err := server.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server component: %w", err)
	}

	// Node-facing proxy (reused unchanged); its predicate skips IBM Cloud, where the server is exposed
	// directly.
	proxy := ignitionproxyv2.NewComponent(defaultIngressDomain)
	if err := proxy.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-server-proxy component: %w", err)
	}

	return nil
}
