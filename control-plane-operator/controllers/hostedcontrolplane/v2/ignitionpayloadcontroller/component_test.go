package ignitionpayloadcontroller

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/api"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"
	"github.com/openshift/hypershift/support/testutil"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"go.uber.org/mock/gomock"
)

func TestOptionsWorkloadIdentity(t *testing.T) {
	g := NewWithT(t)
	o := &Options{}
	g.Expect(o.IsRequestServing()).To(BeFalse())
	g.Expect(o.MultiZoneSpread()).To(BeTrue())
	g.Expect(o.NeedsManagementKASAccess()).To(BeTrue())
}

func TestPredicate(t *testing.T) {
	testCases := []struct {
		name     string
		enabled  bool
		expected bool
	}{
		{name: "gate OFF -> false", enabled: false, expected: false},
		{name: "gate ON -> true", enabled: true, expected: true},
		{
			// The predicate reads only Enabled, never the HCP DisableIgnitionServerAnnotation (which
			// also carries the HO's cutover signal). Operator-disable is folded into Enabled upstream.
			name:     "gate ON, HCP DisableIgnitionServerAnnotation set -> still true (predicate ignores it)",
			enabled:  true,
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: "test-ns", Annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"}},
			}
			result, err := (&Options{Enabled: tc.enabled}).predicate(component.WorkloadContext{HCP: hcp})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(result).To(Equal(tc.expected))
		})
	}
}

func TestReconcileRendersController(t *testing.T) {
	g := NewWithT(t)
	const ns = "hcp"

	ctrl := gomock.NewController(t)
	mockRelease := releaseinfo.NewMockProviderWithOpenShiftImageRegistryOverrides(ctrl)
	mockRelease.EXPECT().GetRegistryOverrides().Return(map[string]string{}).AnyTimes()
	mockRelease.EXPECT().GetOpenShiftImageRegistryOverrides().Return(map[string][]string{}).AnyTimes()

	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: ns},
		Spec: hyperv1.HostedControlPlaneSpec{
			ReleaseImage: "quay.io/openshift-release-dev/ocp-release:4.18.0-x86_64",
			Platform:     hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
		},
	}

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

	g.Expect(NewComponent(&Options{HyperShiftOperatorImage: "test-ho-image", ReleaseProvider: mockRelease}).Reconcile(cpContext)).To(Succeed())

	// Deployment: HO image, subcommand + feature-gate + platform args, generator scaffolding.
	dep := &appsv1.Deployment{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, dep)).To(Succeed())
	g.Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
	g.Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal(ComponentName))

	c := containerByName(dep.Spec.Template.Spec.Containers, ComponentName)
	g.Expect(c).ToNot(BeNil())
	g.Expect(c.Image).To(Equal("test-ho-image"))
	g.Expect(c.Command).To(Equal([]string{"/usr/bin/hypershift-operator"}))
	g.Expect(c.Args).To(ContainElement("ignition-payload-controller"))
	g.Expect(c.Args).To(ContainElement("--feature-gate-manifest=/shared/99_feature-gate.yaml"))
	g.Expect(c.Args).To(ContainElement("--platform"))
	g.Expect(volumeNames(dep)).To(ContainElements("payloads", "shared"))
	g.Expect(initContainerNames(dep)).To(ContainElement("fetch-feature-gate"))
	// No serving-cert on the generator (it renders payloads; it does not serve ignition).
	g.Expect(volumeNames(dep)).ToNot(ContainElement("serving-cert"))

	// Metrics: the generator's controller-runtime manager serves /metrics on :8080; the container
	// exposes that port and a PodMonitor scrapes it.
	metricsPort := containerPortByName(c, "metrics")
	g.Expect(metricsPort).ToNot(BeNil())
	g.Expect(metricsPort.ContainerPort).To(Equal(int32(8080)))

	pm := &prometheusoperatorv1.PodMonitor{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, pm)).To(Succeed())
	g.Expect(pm.Spec.NamespaceSelector.MatchNames).To(ConsistOf(ns))
	g.Expect(pm.Spec.PodMetricsEndpoints).ToNot(BeEmpty())

	// Role: leader-election lease + ignitionpayloads + secrets.
	role := &rbacv1.Role{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, role)).To(Succeed())
	g.Expect(roleGrants(role, "coordination.k8s.io", "leases")).To(BeTrue())
	g.Expect(roleGrants(role, "hypershift.openshift.io", "ignitionpayloads")).To(BeTrue())
	g.Expect(roleGrants(role, "", "secrets")).To(BeTrue())
}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func containerPortByName(c *corev1.Container, name string) *corev1.ContainerPort {
	for i := range c.Ports {
		if c.Ports[i].Name == name {
			return &c.Ports[i]
		}
	}
	return nil
}

func volumeNames(d *appsv1.Deployment) []string {
	var out []string
	for _, v := range d.Spec.Template.Spec.Volumes {
		out = append(out, v.Name)
	}
	return out
}

func initContainerNames(d *appsv1.Deployment) []string {
	var out []string
	for _, c := range d.Spec.Template.Spec.InitContainers {
		out = append(out, c.Name)
	}
	return out
}

func roleGrants(role *rbacv1.Role, group, resource string) bool {
	for _, r := range role.Rules {
		if sets.New(r.APIGroups...).Has(group) && sets.New(r.Resources...).Has(resource) {
			return true
		}
	}
	return false
}
