package hostedcluster

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadcontroller"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserverproxy"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"
	"github.com/openshift/hypershift/support/api"
	controlplanecomponent "github.com/openshift/hypershift/support/controlplane-component"

	configv1 "github.com/openshift/api/config/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIgnitionEndpointResources(t *testing.T) {
	const ns = "hcp-ns"
	tests := []struct {
		name                  string
		cutoverActive         bool
		wantRouteName         string
		wantProxyServiceName  string
		wantServerServiceName string
	}{
		{
			name:                  "cutover inactive -> legacy ignition-server names",
			cutoverActive:         false,
			wantRouteName:         ignitionserver.Route(ns).Name,
			wantProxyServiceName:  ignitionserver.ProxyService(ns).Name,
			wantServerServiceName: ignitionserver.Service(ns).Name,
		},
		{
			name:                  "cutover active -> new ignition-payload-server names",
			cutoverActive:         true,
			wantRouteName:         ignitionpayloadserver.ComponentName,
			wantProxyServiceName:  ignitionpayloadserverproxy.ComponentName,
			wantServerServiceName: ignitionpayloadserver.ComponentName,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			route, proxyService, serverService := ignitionEndpointResources(tc.cutoverActive, ns)

			g.Expect(route.Name).To(Equal(tc.wantRouteName))
			g.Expect(route.Namespace).To(Equal(ns))
			g.Expect(proxyService.Name).To(Equal(tc.wantProxyServiceName))
			g.Expect(proxyService.Namespace).To(Equal(ns))
			g.Expect(serverService.Name).To(Equal(tc.wantServerServiceName))
			g.Expect(serverService.Namespace).To(Equal(ns))
		})
	}
}

func serverComponent(ns string, available bool) *hyperv1.ControlPlaneComponent {
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	return &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ignitionpayloadserver.ComponentName},
		Status: hyperv1.ControlPlaneComponentStatus{
			Conditions: []metav1.Condition{
				{Type: string(hyperv1.ControlPlaneComponentAvailable), Status: status, Reason: "Test"},
			},
		},
	}
}

func TestIgnitionPayloadCutoverActive(t *testing.T) {
	const hcpNs = "clusters-test"

	tests := []struct {
		name       string
		featureSet configv1.FeatureSet
		serverCR   *hyperv1.ControlPlaneComponent
		want       bool
	}{
		{
			name:       "gate OFF -> inactive (no CR read)",
			featureSet: configv1.Default,
			serverCR:   serverComponent(hcpNs, true),
			want:       false,
		},
		{
			name:       "gate ON, server CR missing -> inactive",
			featureSet: configv1.TechPreviewNoUpgrade,
			want:       false,
		},
		{
			name:       "gate ON, server not available -> inactive",
			featureSet: configv1.TechPreviewNoUpgrade,
			serverCR:   serverComponent(hcpNs, false),
			want:       false,
		},
		{
			name:       "gate ON, server available -> active",
			featureSet: configv1.TechPreviewNoUpgrade,
			serverCR:   serverComponent(hcpNs, true),
			want:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			previous := featuregate.FeatureSet()
			featuregate.ConfigureFeatureSet(string(tc.featureSet))
			t.Cleanup(func() { featuregate.ConfigureFeatureSet(string(previous)) })

			builder := fake.NewClientBuilder().WithScheme(api.Scheme)
			if tc.serverCR != nil {
				builder = builder.WithObjects(tc.serverCR)
			}
			fakeClient := builder.Build()

			active, err := ignitionPayloadCutoverActive(ctx, fakeClient, hcpNs)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(active).To(Equal(tc.want))
		})
	}
}

// TestReconcileIgnitionPayloadComponentsDisabled proves the wrapper is a no-op when the components are
// disabled — either because the gate is off, or because the operator set DisableIgnitionServerAnnotation
// on the HostedCluster (no ignition at all). In both cases Enabled is false, so the always-invoked
// wrapper reconciles the three components down (predicate-false delete) without error and creates no CRs.
func TestReconcileIgnitionPayloadComponentsDisabled(t *testing.T) {
	const hcpNs = "clusters-test"

	tests := []struct {
		name          string
		featureSet    configv1.FeatureSet
		hcAnnotations map[string]string
	}{
		{
			name:       "gate OFF -> no components",
			featureSet: configv1.Default,
		},
		{
			name:          "gate ON but operator disabled ignition on the HC -> no components",
			featureSet:    configv1.TechPreviewNoUpgrade,
			hcAnnotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			previous := featuregate.FeatureSet()
			featuregate.ConfigureFeatureSet(string(tc.featureSet))
			t.Cleanup(func() { featuregate.ConfigureFeatureSet(string(previous)) })

			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: hcpNs},
				Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(hcp).Build()
			fetched := &hyperv1.HostedControlPlane{}
			g.Expect(fakeClient.Get(ctx, crclient.ObjectKeyFromObject(hcp), fetched)).To(Succeed())

			cpContext := controlplanecomponent.ControlPlaneContext{Context: ctx, Client: fakeClient, HCP: fetched}
			r := &HostedClusterReconciler{Client: fakeClient}
			hcluster := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "clusters", Annotations: tc.hcAnnotations}}

			g.Expect(r.reconcileIgnitionPayloadComponents(cpContext, hcluster, "ho-image", nil, "")).To(Succeed())

			// No new-stack ControlPlaneComponent CRs exist (nothing was created).
			for _, name := range []string{ignitionpayloadcontroller.ComponentName, ignitionpayloadserver.ComponentName, ignitionpayloadserverproxy.ComponentName} {
				cpc := &hyperv1.ControlPlaneComponent{}
				err := fakeClient.Get(ctx, crclient.ObjectKey{Namespace: hcpNs, Name: name}, cpc)
				g.Expect(crclient.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "expected %s ControlPlaneComponent to be absent", name)
			}
		})
	}
}
