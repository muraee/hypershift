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
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserverproxy"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
