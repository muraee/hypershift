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
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadcontroller"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserverproxy"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"
	controlplanecomponent "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hasDisableIgnitionServerAnnotation reports whether the operator set DisableIgnitionServerAnnotation
// on the HostedCluster, i.e. requested no ignition at all. This reads the HostedCluster (operator
// intent), which is distinct from the same annotation on the HostedControlPlane — the HO also sets
// the HCP annotation purely as the legacy-standdown cutover signal, so the HCP value cannot be used
// to infer operator intent.
func hasDisableIgnitionServerAnnotation(hcluster *hyperv1.HostedCluster) bool {
	_, ok := hcluster.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return ok
}

// ignitionPayloadCutoverActive reports whether the data plane has cut over to the re-architected
// ignition payload stack for this HostedControlPlane. It is true when the IgnitionPayloadSystem
// management feature gate is enabled AND the new ignition-payload-server ControlPlaneComponent
// reports Available. This drives both endpoint re-pointing and the legacy-standdown signal, so the
// legacy ignition-server is only disabled once the new server is actually serving (no serving gap).
func ignitionPayloadCutoverActive(ctx context.Context, c client.Client, hcpNamespace string) (bool, error) {
	if !featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem) {
		return false, nil
	}
	cr := &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: hcpNamespace, Name: ignitionpayloadserver.ComponentName},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get %s ControlPlaneComponent: %w", ignitionpayloadserver.ComponentName, err)
	}
	return meta.IsStatusConditionTrue(cr.Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable)), nil
}

// ignitionEndpointResources returns the empty Route, proxy Service, and backend server Service
// objects (name + namespace only) whose cluster state the ignition endpoint is derived from. When
// the cutover to the re-architected stack is active the ignition-payload-* names are returned;
// otherwise the legacy ignition-server names. The two endpoint-derivation blocks
// (hostedcluster_controller.go and reconcile_legacy.go) share this helper so their resource
// selection cannot drift.
func ignitionEndpointResources(cutoverActive bool, namespace string) (route *routev1.Route, proxyService *corev1.Service, serverService *corev1.Service) {
	if cutoverActive {
		route = &routev1.Route{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: ignitionpayloadserver.ComponentName}}
		proxyService = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: ignitionpayloadserverproxy.ComponentName}}
		serverService = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: ignitionpayloadserver.ComponentName}}
		return route, proxyService, serverService
	}
	return ignitionserver.Route(namespace), ignitionserver.ProxyService(namespace), ignitionserver.Service(namespace)
}

// reconcileIgnitionPayloadComponents reconciles the three re-architected ignition components
// (ignition-payload-controller, ignition-payload-server, and the proxy) from the HyperShift
// Operator image, gated on the IgnitionPayloadSystem management feature gate. It mirrors
// reconcileKarpenterOperator: the gate state is threaded into each component's Options.Enabled and
// all three are always reconciled so that flipping the gate off tears them down via the
// predicate-false delete path. When the gate is off and the components were never created, these are
// no-op deletes, so the gate-off path leaves cluster state unchanged.
//
// The legacy-standdown signal is handled separately (the HO sets the existing
// DisableIgnitionServerAnnotation on the HCP once cutover is active, in
// reconcileHostedControlPlaneAnnotations) rather than via a new annotation an older in-cluster CPO
// would not understand during an N->N+1 upgrade.
//
// Enabled encodes BOTH the management gate AND the per-HostedCluster operator intent: if the operator
// set DisableIgnitionServerAnnotation on the HostedCluster ("no ignition at all"), Enabled is false so
// the new components are torn down — and the annotation mirror loop also disables the legacy stack, so
// both are off. The component predicates key ONLY on this Enabled bool, never on the HCP annotation
// (which also carries the HO's cutover signal and must not disable the new components).
func (r *HostedClusterReconciler) reconcileIgnitionPayloadComponents(cpContext controlplanecomponent.ControlPlaneContext, hcluster *hyperv1.HostedCluster, hypershiftOperatorImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides, defaultIngressDomain string) error {
	enabled := featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem) && !hasDisableIgnitionServerAnnotation(hcluster)

	controller := ignitionpayloadcontroller.NewComponent(&ignitionpayloadcontroller.Options{
		HyperShiftOperatorImage: hypershiftOperatorImage,
		ReleaseProvider:         releaseProvider,
		Enabled:                 enabled,
	})
	if err := controller.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-payload-controller component: %w", err)
	}

	server := ignitionpayloadserver.NewComponent(&ignitionpayloadserver.Options{
		HyperShiftOperatorImage: hypershiftOperatorImage,
		DefaultIngressDomain:    defaultIngressDomain,
		Enabled:                 enabled,
	})
	if err := server.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-payload-server component: %w", err)
	}

	proxy := ignitionpayloadserverproxy.NewComponent(&ignitionpayloadserverproxy.Options{
		Enabled: enabled,
	})
	if err := proxy.Reconcile(cpContext); err != nil {
		return fmt.Errorf("failed to reconcile ignition-payload-server-proxy component: %w", err)
	}

	return nil
}
