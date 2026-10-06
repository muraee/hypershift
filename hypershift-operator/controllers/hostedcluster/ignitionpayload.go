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

// useIgnitionPayloadSystem reports whether the re-architected ignition payload stack
// (ignition-payload-controller + ignition-payload-server + proxy) should back this
// HostedCluster's ignition endpoint. It is true when the IgnitionPayloadSystem management
// feature gate is enabled and ignition is not disabled entirely via the
// DisableIgnitionServerAnnotation. When false, the legacy ignition-server stack is used.
func useIgnitionPayloadSystem(hcluster *hyperv1.HostedCluster) bool {
	if _, disabled := hcluster.Annotations[hyperv1.DisableIgnitionServerAnnotation]; disabled {
		return false
	}
	return featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem)
}

// ignitionEndpointResources returns the empty Route, proxy Service, and backend server
// Service objects (name + namespace only) whose cluster state the ignition endpoint is
// derived from. When useIgnitionPayloadSystem is true the re-architected ignition-payload-*
// stack names are returned; otherwise the legacy ignition-server stack names. The two
// endpoint-derivation blocks (hostedcluster_controller.go and reconcile_legacy.go) share
// this helper so their resource selection cannot drift.
func ignitionEndpointResources(hcluster *hyperv1.HostedCluster, namespace string) (route *routev1.Route, proxyService *corev1.Service, serverService *corev1.Service) {
	if useIgnitionPayloadSystem(hcluster) {
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
// After reconciling, it maintains the IgnitionPayloadActiveAnnotation on the HCP (the
// legacy-standdown signal): set to "true" only once the new ignition-payload-server reports
// Available, removed otherwise. The legacy ignition-server/proxy predicates stand down while it is
// set, so the legacy stack is torn down only after the new one serves (no serving gap).
func (r *HostedClusterReconciler) reconcileIgnitionPayloadComponents(cpContext controlplanecomponent.ControlPlaneContext, hypershiftOperatorImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides, defaultIngressDomain string) error {
	enabled := featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem)

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

	return r.reconcileIgnitionPayloadActiveAnnotation(cpContext, enabled)
}

// reconcileIgnitionPayloadActiveAnnotation maintains the IgnitionPayloadActiveAnnotation on the
// HostedControlPlane. It is set to "true" only when the IgnitionPayloadSystem gate is on AND the new
// ignition-payload-server ControlPlaneComponent reports Available, and removed otherwise. This is the
// legacy-standdown signal consumed by the legacy ignition-server/proxy component predicates, so the
// legacy stack is torn down only once the new server is serving.
func (r *HostedClusterReconciler) reconcileIgnitionPayloadActiveAnnotation(cpContext controlplanecomponent.ControlPlaneContext, gateEnabled bool) error {
	serverAvailable := false
	if gateEnabled {
		cr := &hyperv1.ControlPlaneComponent{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cpContext.HCP.Namespace,
				Name:      ignitionpayloadserver.ComponentName,
			},
		}
		if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(cr), cr); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to get %s ControlPlaneComponent: %w", ignitionpayloadserver.ComponentName, err)
			}
		} else {
			serverAvailable = meta.IsStatusConditionTrue(cr.Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable))
		}
	}

	active := gateEnabled && serverAvailable

	hcp := cpContext.HCP
	if active {
		if hcp.Annotations[hyperv1.IgnitionPayloadActiveAnnotation] == "true" {
			return nil
		}
	} else if _, present := hcp.Annotations[hyperv1.IgnitionPayloadActiveAnnotation]; !present {
		return nil
	}

	original := hcp.DeepCopy()
	if active {
		if hcp.Annotations == nil {
			hcp.Annotations = map[string]string{}
		}
		hcp.Annotations[hyperv1.IgnitionPayloadActiveAnnotation] = "true"
	} else {
		delete(hcp.Annotations, hyperv1.IgnitionPayloadActiveAnnotation)
	}
	// Metadata (not status) patch, so the hcpstatuspatch linter does not apply; the optimistic lock
	// mirrors the HCP finalizer patch in reconcileKarpenterOperator and surfaces concurrent writes
	// as a conflict (retried on the next reconcile) rather than silently clobbering them.
	if err := cpContext.Client.Patch(cpContext, hcp, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to patch HostedControlPlane %s annotation: %w", hyperv1.IgnitionPayloadActiveAnnotation, err)
	}
	return nil
}
