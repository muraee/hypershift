package ignitionpayloadserver

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/api"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/testutil"
	"github.com/openshift/hypershift/support/upsert"

	routev1 "github.com/openshift/api/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestReconcileRendersServer(t *testing.T) {
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

	g.Expect(NewComponent(&Options{HyperShiftOperatorImage: "test-ho-image", DefaultIngressDomain: "apps.example.com"}).Reconcile(cpContext)).To(Succeed())

	// Deployment: HO image, subcommand + cert args, serving scaffolding (no generator pieces).
	dep := &appsv1.Deployment{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, dep)).To(Succeed())
	g.Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
	g.Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal(ComponentName))

	c := containerByName(dep.Spec.Template.Spec.Containers, ComponentName)
	g.Expect(c).ToNot(BeNil())
	g.Expect(c.Image).To(Equal("test-ho-image"))
	g.Expect(c.Command).To(Equal([]string{"/usr/bin/hypershift-operator"}))
	g.Expect(c.Args).To(ContainElement("ignition-payload-server"))
	g.Expect(c.Args).To(ContainElement("--cert-file"))
	g.Expect(c.Args).To(ContainElement("/var/run/secrets/ignition/serving-cert/tls.crt"))
	g.Expect(c.Args).To(ContainElement("--key-file"))
	g.Expect(c.Args).To(ContainElement("/var/run/secrets/ignition/serving-cert/tls.key"))
	g.Expect(portNumbers(c)).To(ContainElement(int32(9090)))
	// The server presents the INTERNAL cert on 9090 (non-IBMCloud).
	g.Expect(servingCertVolumeSecret(dep)).To(Equal(tlsCertSecretName))
	// No generator scaffolding on the serving tier.
	g.Expect(volumeNames(dep)).ToNot(ContainElement("payloads"))
	g.Expect(initContainerNames(dep)).ToNot(ContainElement("fetch-feature-gate"))

	// Service: ignition-payload-server, 443 -> 9090.
	svc := &corev1.Service{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, svc)).To(Succeed())
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(443)))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(9090))

	// Route: ignition-payload-server exists (publishing strategy is Route).
	route := &routev1.Route{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, route)).To(Succeed())

	// Role: secrets + ignitionpayloads(+status) only; no hostedcontrolplanes.
	role := &rbacv1.Role{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, role)).To(Succeed())
	g.Expect(roleGrants(role, "", "secrets")).To(BeTrue())
	g.Expect(roleGrants(role, "hypershift.openshift.io", "ignitionpayloads")).To(BeTrue())
	g.Expect(roleGrants(role, "hypershift.openshift.io", "ignitionpayloads/status")).To(BeTrue())
	g.Expect(roleGrants(role, "hypershift.openshift.io", "hostedcontrolplanes")).To(BeFalse())
}

func TestIBMCloudServerPresentsNodeFacingCert(t *testing.T) {
	g := NewWithT(t)
	const ns = "hcp"

	hcp := ignitionRouteHCP(ns)
	hcp.Spec.Platform.Type = hyperv1.IBMCloudPlatform

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

	g.Expect(NewComponent(&Options{HyperShiftOperatorImage: "test-ho-image", DefaultIngressDomain: "apps.example.com"}).Reconcile(cpContext)).To(Succeed())

	dep := &appsv1.Deployment{}
	g.Expect(cpContext.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ComponentName}, dep)).To(Succeed())
	// On IBMCloud there is no proxy, so the server presents the node-facing serving cert directly.
	g.Expect(servingCertVolumeSecret(dep)).To(Equal(servingCertSecretName))
}

func TestPKIAdapters(t *testing.T) {
	g := NewWithT(t)
	const ns = "hcp"

	hcp := ignitionRouteHCP(ns)

	// Seed an admitted Route so the node-facing serving cert can resolve its host SAN.
	const routeHost = "ignition.apps.example.com"
	admittedRoute := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ComponentName},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{{Host: routeHost}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(admittedRoute).Build()

	wctx := component.WorkloadContext{
		Context:                t.Context(),
		HCP:                    hcp,
		Client:                 fakeClient,
		SkipCertificateSigning: false,
	}

	// CA: self-signed.
	caSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: caCertSecretName}}
	g.Expect(adaptCACertSecret(wctx, caSecret)).To(Succeed())
	g.Expect(caSecret.Data).To(HaveKey(corev1.TLSCertKey))
	g.Expect(caSecret.Data).To(HaveKey(corev1.TLSPrivateKeyKey))
	caCert := parseCert(g, caSecret.Data[corev1.TLSCertKey])
	g.Expect(caCert.IsCA).To(BeTrue())

	// The CA must exist in the cluster for the signed-cert adapters to resolve it.
	g.Expect(fakeClient.Create(t.Context(), caSecret.DeepCopy())).To(Succeed())

	// Node-facing serving cert: SAN = the Route host.
	servingSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: servingCertSecretName}}
	g.Expect(adaptServingCertSecret(wctx, servingSecret)).To(Succeed())
	servingCert := parseCert(g, servingSecret.Data[corev1.TLSCertKey])
	g.Expect(servingCert.DNSNames).To(ContainElement(routeHost))
	g.Expect(servingCert.CheckSignatureFrom(caCert)).To(Succeed())

	// Internal server cert: SANs = the Service DNS names, no Route dependency.
	tlsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tlsCertSecretName}}
	g.Expect(adaptServerTLSSecret(wctx, tlsSecret)).To(Succeed())
	tlsCert := parseCert(g, tlsSecret.Data[corev1.TLSCertKey])
	g.Expect(tlsCert.DNSNames).To(ContainElements(
		ComponentName,
		ComponentName+"."+ns+".svc",
		ComponentName+"."+ns+".svc.cluster.local",
	))
	g.Expect(tlsCert.CheckSignatureFrom(caCert)).To(Succeed())
}

func parseCert(g *WithT, pemBytes []byte) *x509.Certificate {
	block, _ := pem.Decode(pemBytes)
	g.Expect(block).ToNot(BeNil())
	cert, err := x509.ParseCertificate(block.Bytes)
	g.Expect(err).ToNot(HaveOccurred())
	return cert
}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func portNumbers(c *corev1.Container) []int32 {
	var out []int32
	for _, p := range c.Ports {
		out = append(out, p.ContainerPort)
	}
	return out
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

func servingCertVolumeSecret(d *appsv1.Deployment) string {
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "serving-cert" && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

func roleGrants(role *rbacv1.Role, group, resource string) bool {
	for _, r := range role.Rules {
		if sets.New(r.APIGroups...).Has(group) && sets.New(r.Resources...).Has(resource) {
			return true
		}
	}
	return false
}
