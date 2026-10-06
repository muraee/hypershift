package hostedcluster

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
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

func TestReconcileIgnitionPayloadActiveAnnotation(t *testing.T) {
	const (
		hcpName = "test-hcp"
		hcpNs   = "clusters-test"
	)

	tests := []struct {
		name               string
		gateEnabled        bool
		serverCR           *hyperv1.ControlPlaneComponent
		initialAnnotations map[string]string
		wantActive         bool // true -> annotation == "true"; false -> annotation absent
	}{
		{
			name:        "gate OFF, no annotation -> stays absent",
			gateEnabled: false,
			wantActive:  false,
		},
		{
			name:               "gate OFF, annotation present -> removed",
			gateEnabled:        false,
			initialAnnotations: map[string]string{hyperv1.IgnitionPayloadActiveAnnotation: "true"},
			wantActive:         false,
		},
		{
			name:        "gate ON, server CR missing -> not active",
			gateEnabled: true,
			wantActive:  false,
		},
		{
			name:        "gate ON, server not available -> not active",
			gateEnabled: true,
			serverCR:    serverComponent(hcpNs, false),
			wantActive:  false,
		},
		{
			name:        "gate ON, server available -> active",
			gateEnabled: true,
			serverCR:    serverComponent(hcpNs, true),
			wantActive:  true,
		},
		{
			name:               "gate ON, server available, annotation already set -> stays active",
			gateEnabled:        true,
			serverCR:           serverComponent(hcpNs, true),
			initialAnnotations: map[string]string{hyperv1.IgnitionPayloadActiveAnnotation: "true"},
			wantActive:         true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: hcpName, Namespace: hcpNs, Annotations: tc.initialAnnotations},
			}
			objs := []crclient.Object{hcp}
			if tc.serverCR != nil {
				objs = append(objs, tc.serverCR)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(objs...).Build()

			// Re-fetch the HCP so cpContext.HCP carries a client-managed resourceVersion for the
			// optimistic-lock patch.
			fetched := &hyperv1.HostedControlPlane{}
			g.Expect(fakeClient.Get(ctx, crclient.ObjectKeyFromObject(hcp), fetched)).To(Succeed())

			cpContext := controlplanecomponent.ControlPlaneContext{Context: ctx, Client: fakeClient, HCP: fetched}
			r := &HostedClusterReconciler{Client: fakeClient}

			g.Expect(r.reconcileIgnitionPayloadActiveAnnotation(cpContext, tc.gateEnabled)).To(Succeed())

			updated := &hyperv1.HostedControlPlane{}
			g.Expect(fakeClient.Get(ctx, crclient.ObjectKeyFromObject(hcp), updated)).To(Succeed())
			if tc.wantActive {
				g.Expect(updated.Annotations).To(HaveKeyWithValue(hyperv1.IgnitionPayloadActiveAnnotation, "true"))
			} else {
				g.Expect(updated.Annotations).ToNot(HaveKey(hyperv1.IgnitionPayloadActiveAnnotation))
			}
		})
	}
}

// TestReconcileIgnitionPayloadComponentsGateOff proves the gate-off path is a no-op: the wrapper is
// always invoked but, with the gate off and nothing previously created, it reconciles the three
// components down (predicate-false delete) without error and leaves the legacy-standdown annotation
// unset.
func TestReconcileIgnitionPayloadComponentsGateOff(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	previous := featuregate.FeatureSet()
	featuregate.ConfigureFeatureSet(string(configv1.Default))
	t.Cleanup(func() { featuregate.ConfigureFeatureSet(string(previous)) })

	const hcpNs = "clusters-test"
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: hcpNs},
		Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(hcp).Build()
	fetched := &hyperv1.HostedControlPlane{}
	g.Expect(fakeClient.Get(ctx, crclient.ObjectKeyFromObject(hcp), fetched)).To(Succeed())

	cpContext := controlplanecomponent.ControlPlaneContext{Context: ctx, Client: fakeClient, HCP: fetched}
	r := &HostedClusterReconciler{Client: fakeClient}

	g.Expect(r.reconcileIgnitionPayloadComponents(cpContext, "ho-image", nil, "")).To(Succeed())

	updated := &hyperv1.HostedControlPlane{}
	g.Expect(fakeClient.Get(ctx, crclient.ObjectKeyFromObject(hcp), updated)).To(Succeed())
	g.Expect(updated.Annotations).ToNot(HaveKey(hyperv1.IgnitionPayloadActiveAnnotation))

	// No new-stack ControlPlaneComponent CRs exist (nothing was created).
	for _, name := range []string{ignitionpayloadserver.ComponentName, ignitionpayloadserverproxy.ComponentName} {
		cpc := &hyperv1.ControlPlaneComponent{}
		err := fakeClient.Get(ctx, crclient.ObjectKey{Namespace: hcpNs, Name: name}, cpc)
		g.Expect(crclient.IgnoreNotFound(err)).To(Succeed())
		g.Expect(err).To(HaveOccurred(), "expected %s ControlPlaneComponent to be absent", name)
	}
}
