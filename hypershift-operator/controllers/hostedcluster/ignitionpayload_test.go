package hostedcluster

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserverproxy"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"

	configv1 "github.com/openshift/api/config/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIgnitionEndpointResources(t *testing.T) {
	const ns = "hcp-ns"
	tests := []struct {
		name                  string
		featureSet            configv1.FeatureSet
		annotations           map[string]string
		wantRouteName         string
		wantProxyServiceName  string
		wantServerServiceName string
	}{
		{
			name:                  "gate OFF -> legacy ignition-server names",
			featureSet:            configv1.Default,
			wantRouteName:         ignitionserver.Route(ns).Name,
			wantProxyServiceName:  ignitionserver.ProxyService(ns).Name,
			wantServerServiceName: ignitionserver.Service(ns).Name,
		},
		{
			name:                  "gate ON -> new ignition-payload-server names",
			featureSet:            configv1.TechPreviewNoUpgrade,
			wantRouteName:         ignitionpayloadserver.ComponentName,
			wantProxyServiceName:  ignitionpayloadserverproxy.ComponentName,
			wantServerServiceName: ignitionpayloadserver.ComponentName,
		},
		{
			name:                  "gate ON but ignition disabled -> legacy ignition-server names",
			featureSet:            configv1.TechPreviewNoUpgrade,
			annotations:           map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"},
			wantRouteName:         ignitionserver.Route(ns).Name,
			wantProxyServiceName:  ignitionserver.ProxyService(ns).Name,
			wantServerServiceName: ignitionserver.Service(ns).Name,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			previous := featuregate.FeatureSet()
			featuregate.ConfigureFeatureSet(string(tc.featureSet))
			t.Cleanup(func() { featuregate.ConfigureFeatureSet(string(previous)) })

			hcluster := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			route, proxyService, serverService := ignitionEndpointResources(hcluster, ns)

			g.Expect(route.Name).To(Equal(tc.wantRouteName))
			g.Expect(route.Namespace).To(Equal(ns))
			g.Expect(proxyService.Name).To(Equal(tc.wantProxyServiceName))
			g.Expect(proxyService.Namespace).To(Equal(ns))
			g.Expect(serverService.Name).To(Equal(tc.wantServerServiceName))
			g.Expect(serverService.Namespace).To(Equal(ns))
		})
	}
}
