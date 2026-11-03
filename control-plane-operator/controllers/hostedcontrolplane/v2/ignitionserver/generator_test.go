package ignitionserver

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/assets"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/api"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/mock/gomock"
)

const testHOImage = "registry.example.com/hypershift-operator:latest"

func ignitionRouteHCP() *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-hcp",
			Namespace: "test-namespace",
		},
		Spec: hyperv1.HostedControlPlaneSpec{
			Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
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

func admittedIgnitionRoute(namespace, host string) *routev1.Route {
	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      ComponentName,
		},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{{Host: host}},
		},
	}
}

func TestResolveIgnitionServerAddress(t *testing.T) {
	t.Run("returns the admitted route host", func(t *testing.T) {
		g := NewWithT(t)
		hcp := ignitionRouteHCP()
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).
			WithObjects(admittedIgnitionRoute(hcp.Namespace, "ignition.example.com")).Build()

		addr, err := resolveIgnitionServerAddress(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(addr).To(Equal("ignition.example.com"))
	})

	t.Run("returns empty when the route is not yet admitted", func(t *testing.T) {
		g := NewWithT(t)
		hcp := ignitionRouteHCP()
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).
			WithObjects(&routev1.Route{ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: ComponentName}}).Build()

		addr, err := resolveIgnitionServerAddress(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(addr).To(BeEmpty())
	})

	t.Run("returns empty when the route does not exist", func(t *testing.T) {
		g := NewWithT(t)
		hcp := ignitionRouteHCP()
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).Build()

		addr, err := resolveIgnitionServerAddress(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(addr).To(BeEmpty())
	})

	t.Run("returns the nodeport address", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: "test-namespace"},
			Spec: hyperv1.HostedControlPlaneSpec{
				Services: []hyperv1.ServicePublishingStrategyMapping{
					{
						Service: hyperv1.Ignition,
						ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{
							Type:     hyperv1.NodePort,
							NodePort: &hyperv1.NodePortPublishingStrategy{Address: "10.0.0.5"},
						},
					},
				},
			},
		}
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).Build()

		addr, err := resolveIgnitionServerAddress(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(addr).To(Equal("10.0.0.5"))
	})

	t.Run("errors when no ignition service strategy is configured", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: "test-namespace"}}
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).Build()

		_, err := resolveIgnitionServerAddress(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp})
		g.Expect(err).To(HaveOccurred())
	})
}

func TestAdaptGeneratorDeployment(t *testing.T) {
	newGenerator := func(t *testing.T) *ignitionServerGenerator {
		ctrl := gomock.NewController(t)
		mockRelease := releaseinfo.NewMockProviderWithOpenShiftImageRegistryOverrides(ctrl)
		mockRelease.EXPECT().GetOpenShiftImageRegistryOverrides().Return(map[string][]string{}).AnyTimes()
		mockRelease.EXPECT().GetRegistryOverrides().Return(map[string]string{}).AnyTimes()
		return &ignitionServerGenerator{hypershiftOperatorImage: testHOImage, releaseProvider: mockRelease}
	}

	t.Run("requeues until the serving route is admitted", func(t *testing.T) {
		g := NewWithT(t)
		hcp := ignitionRouteHCP()
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).Build()

		deployment, err := assets.LoadDeploymentManifest(GeneratorComponentName)
		g.Expect(err).ToNot(HaveOccurred())

		err = newGenerator(t).adaptGeneratorDeployment(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp}, deployment)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("configures the container once the endpoint is available", func(t *testing.T) {
		g := NewWithT(t)
		hcp := ignitionRouteHCP()
		client := crfake.NewClientBuilder().WithScheme(api.Scheme).
			WithObjects(admittedIgnitionRoute(hcp.Namespace, "ignition.example.com")).Build()

		deployment, err := assets.LoadDeploymentManifest(GeneratorComponentName)
		g.Expect(err).ToNot(HaveOccurred())

		err = newGenerator(t).adaptGeneratorDeployment(component.WorkloadContext{Context: t.Context(), Client: client, HCP: hcp}, deployment)
		g.Expect(err).ToNot(HaveOccurred())

		var container *corev1.Container
		for i := range deployment.Spec.Template.Spec.Containers {
			if deployment.Spec.Template.Spec.Containers[i].Name == GeneratorComponentName {
				container = &deployment.Spec.Template.Spec.Containers[i]
			}
		}
		g.Expect(container).ToNot(BeNil())
		g.Expect(container.Image).To(Equal(testHOImage))
		g.Expect(container.Args).To(ContainElements("--ignition-server-endpoint", "ignition.example.com"))
		g.Expect(container.Args).To(ContainElements("--platform", string(hyperv1.AWSPlatform)))

		var hasImgOverrides bool
		for _, env := range container.Env {
			if env.Name == "OPENSHIFT_IMG_OVERRIDES" {
				hasImgOverrides = true
			}
		}
		g.Expect(hasImgOverrides).To(BeTrue())
	})
}

func TestAdaptTokenSigningKeySecret(t *testing.T) {
	t.Run("mints a keypair both halves can parse", func(t *testing.T) {
		g := NewWithT(t)
		secret := &corev1.Secret{}

		g.Expect(adaptTokenSigningKeySecret(component.WorkloadContext{Context: t.Context()}, secret)).To(Succeed())
		g.Expect(secret.Type).To(Equal(corev1.SecretTypeTLS))

		// The generator parses the SEC1 private key under tls.key; the serving tier parses the public
		// key from the certificate under tls.crt. Both must succeed on the same minted Secret.
		priv, err := jwt.ParseECPrivateKeyFromPEM(secret.Data[corev1.TLSPrivateKeyKey])
		g.Expect(err).ToNot(HaveOccurred())
		pub, err := jwt.ParseECPublicKeyFromPEM(secret.Data[corev1.TLSCertKey])
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(priv.PublicKey.Equal(pub)).To(BeTrue())
	})

	t.Run("is create-only once populated", func(t *testing.T) {
		g := NewWithT(t)
		secret := &corev1.Secret{}
		g.Expect(adaptTokenSigningKeySecret(component.WorkloadContext{Context: t.Context()}, secret)).To(Succeed())

		existingKey := secret.Data[corev1.TLSPrivateKeyKey]
		existingCert := secret.Data[corev1.TLSCertKey]

		g.Expect(adaptTokenSigningKeySecret(component.WorkloadContext{Context: t.Context()}, secret)).To(Succeed())
		g.Expect(secret.Data[corev1.TLSPrivateKeyKey]).To(Equal(existingKey))
		g.Expect(secret.Data[corev1.TLSCertKey]).To(Equal(existingCert))
	})

	t.Run("is a no-op when certificate signing is skipped", func(t *testing.T) {
		g := NewWithT(t)
		secret := &corev1.Secret{}
		g.Expect(adaptTokenSigningKeySecret(component.WorkloadContext{Context: t.Context(), SkipCertificateSigning: true}, secret)).To(Succeed())
		g.Expect(secret.Data).To(BeEmpty())
	})
}
