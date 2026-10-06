package ignitionpayloadserverproxy

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/api"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/testutil"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOptionsWorkloadIdentity(t *testing.T) {
	g := NewWithT(t)
	o := &Options{}
	g.Expect(o.IsRequestServing()).To(BeTrue())
	g.Expect(o.MultiZoneSpread()).To(BeTrue())
	g.Expect(o.NeedsManagementKASAccess()).To(BeFalse())
}

func TestPredicate(t *testing.T) {
	testCases := []struct {
		name        string
		enabled     bool
		platform    hyperv1.PlatformType
		annotations map[string]string
		expected    bool
	}{
		{name: "gate OFF -> false", enabled: false, platform: hyperv1.AWSPlatform, expected: false},
		{name: "gate ON, AWS, ignition enabled -> true", enabled: true, platform: hyperv1.AWSPlatform, expected: true},
		{name: "gate ON, Azure, ignition enabled -> true", enabled: true, platform: hyperv1.AzurePlatform, expected: true},
		{name: "gate ON, IBMCloud -> false (server exposed directly)", enabled: true, platform: hyperv1.IBMCloudPlatform, expected: false},
		{
			name:        "gate ON, DisableIgnitionServerAnnotation -> false",
			enabled:     true,
			platform:    hyperv1.AWSPlatform,
			annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"},
			expected:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: "test-ns", Annotations: tc.annotations},
				Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: tc.platform}},
			}
			result, err := (&Options{Enabled: tc.enabled}).predicate(component.WorkloadContext{HCP: hcp})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(result).To(Equal(tc.expected))
		})
	}
}

func TestAdaptHAProxyConfig(t *testing.T) {
	g := NewWithT(t)
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "hcp"},
	}
	cm := &corev1.ConfigMap{}
	g.Expect(adaptHAProxyConfig(component.WorkloadContext{HCP: hcp}, cm)).To(Succeed())

	conf := cm.Data["haproxy.conf"]
	// The backend targets the new server Service and verifies it against the self-contained CA.
	g.Expect(conf).To(ContainSubstring("server ignition-payload-server ignition-payload-server:443 check ssl ca-file /etc/ssl/ca/ca.crt"))
	// No reference to the legacy ignition-server backend or the CPO root-ca.
	g.Expect(conf).ToNot(ContainSubstring("ignition-server:443"))
	g.Expect(conf).ToNot(ContainSubstring("root-ca"))
}

func ignitionRouteHCP(ns string) *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: ns},
		Spec: hyperv1.HostedControlPlaneSpec{
			ReleaseImage: "quay.io/openshift-release-dev/ocp-release:4.18.0-x86_64",
			Platform:     hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
			Services: []hyperv1.ServicePublishingStrategyMapping{
				{
					Service: hyperv1.Ignition,
					ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{
						Type: hyperv1.Route,
					},
				},
			},
		},
	}
}

// TestReconcileWaitsForServerDependency proves WithDependencies includes the server: without the
// server component available, the proxy does not render its workload and reports WaitingForDependencies.
func TestReconcileWaitsForServerDependency(t *testing.T) {
	g := NewWithT(t)
	const ns = "hcp"
	hcp := ignitionRouteHCP(ns)

	cpContext := component.ControlPlaneContext{
		Context:                t.Context(),
		HCP:                    hcp,
		Client:                 fake.NewClientBuilder().WithScheme(api.Scheme).Build(),
		ApplyProvider:          upsert.NewApplyProvider(false),
		ReleaseImageProvider:   testutil.FakeImageProvider(),
		SkipPredicate:          true,
		SkipCertificateSigning: true,
		OmitOwnerReference:     true,
	}

	g.Expect(NewComponent(&Options{}).Reconcile(cpContext)).To(Succeed())

	// The workload must NOT be rendered while the dependency is unavailable.
	dep := &appsv1.Deployment{}
	err := cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, dep)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	// The component CR must report it is waiting on the ignition-payload-server dependency.
	cpc := &hyperv1.ControlPlaneComponent{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, cpc)).To(Succeed())
	cond := meta.FindStatusCondition(cpc.Status.Conditions, string(hyperv1.ControlPlaneComponentRolloutComplete))
	g.Expect(cond).ToNot(BeNil())
	g.Expect(cond.Reason).To(Equal(hyperv1.WaitingForDependenciesReason))
	g.Expect(cond.Message).To(ContainSubstring(ignitionpayloadserver.ComponentName))
}

func TestReconcileRendersProxy(t *testing.T) {
	g := NewWithT(t)
	const ns = "hcp"
	hcp := ignitionRouteHCP(ns)

	// Seed the server component as available + rolled out at the desired version so the dependency
	// is satisfied and the proxy workload renders.
	serverCPC := &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ignitionpayloadserver.ComponentName},
		Status: hyperv1.ControlPlaneComponentStatus{
			Version: "4.18.0",
			Conditions: []metav1.Condition{
				{Type: string(hyperv1.ControlPlaneComponentAvailable), Status: metav1.ConditionTrue, Reason: "AsExpected"},
				{Type: string(hyperv1.ControlPlaneComponentRolloutComplete), Status: metav1.ConditionTrue, Reason: "AsExpected"},
			},
		},
	}

	cpContext := component.ControlPlaneContext{
		Context:                t.Context(),
		HCP:                    hcp,
		Client:                 fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(serverCPC).Build(),
		ApplyProvider:          upsert.NewApplyProvider(false),
		ReleaseImageProvider:   testutil.FakeImageProvider(),
		SkipPredicate:          true,
		SkipCertificateSigning: true,
		OmitOwnerReference:     true,
	}

	g.Expect(NewComponent(&Options{}).Reconcile(cpContext)).To(Succeed())

	// Deployment: haproxy container, distinct name, node-facing cert + self-contained CA volumes.
	dep := &appsv1.Deployment{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, dep)).To(Succeed())
	g.Expect(containerByName(dep.Spec.Template.Spec.Containers, "haproxy")).ToNot(BeNil())
	g.Expect(volumeSecret(dep, "serving-cert")).To(Equal("ignition-payload-server-serving-cert"))
	g.Expect(volumeSecret(dep, "ca")).To(Equal("ignition-payload-server-ca-cert"))

	// Service: ignition-payload-server-proxy, 443 -> 8443 (targetPort https).
	svc := &corev1.Service{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, svc)).To(Succeed())
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(443)))
	g.Expect(svc.Spec.Ports[0].TargetPort.StrVal).To(Equal("https"))

	// haproxy-config ConfigMap: backend targets the new server.
	cm := &corev1.ConfigMap{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "ignition-payload-server-proxy-config"}, cm)).To(Succeed())
	g.Expect(cm.Data["haproxy.conf"]).To(ContainSubstring("ignition-payload-server:443"))
}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func volumeSecret(d *appsv1.Deployment, volName string) string {
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == volName && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}
