package ignitionserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/certs"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/imageregistry"
	"github.com/openshift/hypershift/support/podspec"
	"github.com/openshift/hypershift/support/proxy"
	"github.com/openshift/hypershift/support/releaseinfo"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// GeneratorComponentName is the leader-elected generation half of the split ignition system. It is
	// a sibling of the serving component (ComponentName): the framework ties a component's name to its
	// asset dir and resource names, so generation and serving cannot share one component.
	GeneratorComponentName = "ignition-server-generator"

	// TokenSigningKeySecretName names the Secret holding the ECDSA keypair the generator signs node
	// bearer tokens with: the SEC1 private key under tls.key (read by the generator) and a self-signed
	// certificate carrying the public key under tls.crt (mounted by the serving tier to verify tokens).
	// It matches the name the ignition-payload-controller subcommand reads at startup.
	TokenSigningKeySecretName = "ignition-payload-token-signing-key"
)

var _ component.ComponentOptions = &ignitionServerGenerator{}

type ignitionServerGenerator struct {
	hypershiftOperatorImage string
	releaseProvider         releaseinfo.ProviderWithOpenShiftImageRegistryOverrides
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (g *ignitionServerGenerator) IsRequestServing() bool {
	return false
}

// MultiZoneSpread implements controlplanecomponent.ComponentOptions. The generator is a singleton
// (leader-elected), so there is nothing to spread across zones.
func (g *ignitionServerGenerator) MultiZoneSpread() bool {
	return false
}

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions. The generator reconciles
// IgnitionPayload resources and the PayloadStore on the management cluster.
func (g *ignitionServerGenerator) NeedsManagementKASAccess() bool {
	return true
}

// NewGeneratorComponent builds the leader-elected ignition payload generator component. It runs the
// HyperShift Operator image's ignition-payload-controller subcommand (generation is an operator
// concern, not a release-payload one), so the image is injected rather than resolved from the payload.
func NewGeneratorComponent(hypershiftOperatorImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides) component.ControlPlaneComponent {
	generator := &ignitionServerGenerator{
		hypershiftOperatorImage: hypershiftOperatorImage,
		releaseProvider:         releaseProvider,
	}

	return component.NewDeploymentComponent(GeneratorComponentName, generator).
		WithAdaptFunction(generator.adaptGeneratorDeployment).
		WithPredicate(predicate).
		WithManifestAdapter(
			"token-signing-key.yaml",
			component.WithAdaptFunction(adaptTokenSigningKeySecret),
			component.DisableIfAnnotationExist(hyperv1.DisablePKIReconciliationAnnotation),
			component.ReconcileExisting(),
		).
		Build()
}

func (g *ignitionServerGenerator) adaptGeneratorDeployment(cpContext component.WorkloadContext, deployment *appsv1.Deployment) error {
	hcp := cpContext.HCP

	// The authored userData points nodes at the serving Route host, so the generator cannot author a
	// usable payload until that host is known. Requeue rather than embed an empty endpoint.
	endpoint, err := resolveIgnitionServerAddress(cpContext)
	if err != nil {
		return err
	}
	if endpoint == "" {
		return fmt.Errorf("ignition server address not yet available; requeuing %s", GeneratorComponentName)
	}

	if err := injectFeatureGateManifest(cpContext, deployment); err != nil {
		return err
	}

	podspec.UpdateContainer(GeneratorComponentName, deployment.Spec.Template.Spec.Containers, func(c *corev1.Container) {
		c.Image = g.hypershiftOperatorImage
		c.Args = append(c.Args,
			"--platform", string(hcp.Spec.Platform.Type),
			"--registry-overrides", imageregistry.ConvertRegistryOverridesToCommandLineFlag(g.releaseProvider.GetRegistryOverrides()),
			"--ignition-server-endpoint", endpoint,
		)

		podspec.UpsertEnvVar(c, corev1.EnvVar{
			Name:  "OPENSHIFT_IMG_OVERRIDES",
			Value: imageregistry.ConvertOpenShiftImageRegistryOverridesToCommandLineFlag(g.releaseProvider.GetOpenShiftImageRegistryOverrides()),
		})

		proxy.SetEnvVars(&c.Env)
	})

	if hcp.Spec.AdditionalTrustBundle != nil {
		podspec.DeploymentAddTrustBundleVolume(hcp.Spec.AdditionalTrustBundle, deployment)
	}

	return nil
}

// adaptTokenSigningKeySecret provisions the ECDSA (ES256) keypair the generator signs node bearer
// tokens with. support/certs is RSA-only, so the keypair is minted here directly: the SEC1 private key
// goes under tls.key (parsed by jwt.ParseECPrivateKeyFromPEM) and a self-signed certificate carrying
// the public key goes under tls.crt (parsed by jwt.ParseECPublicKeyFromPEM). It is create-only —
// rotating the key would invalidate every already-authored node token — so once the Secret is populated
// (ReconcileExisting passes the existing object) it is left untouched.
func adaptTokenSigningKeySecret(cpContext component.WorkloadContext, secret *corev1.Secret) error {
	if cpContext.SkipCertificateSigning {
		return nil
	}
	if len(secret.Data[corev1.TLSPrivateKeyKey]) > 0 && len(secret.Data[corev1.TLSCertKey]) > 0 {
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), certs.Reader())
	if err != nil {
		return fmt.Errorf("failed to generate ECDSA token-signing key: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to marshal ECDSA token-signing key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	serial, err := rand.Int(certs.Reader(), new(big.Int).SetInt64(math.MaxInt64))
	if err != nil {
		return fmt.Errorf("failed to generate token-signing certificate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         "ignition-payload-token-signing",
			OrganizationalUnit: []string{"openshift"},
		},
		NotBefore:             now,
		NotAfter:              now.Add(certs.ValidityTenYears),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(certs.Reader(), template, template, key.Public(), key)
	if err != nil {
		return fmt.Errorf("failed to create self-signed token-signing certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	secret.Type = corev1.SecretTypeTLS
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[corev1.TLSPrivateKeyKey] = keyPEM
	secret.Data[corev1.TLSCertKey] = certPEM
	return nil
}
