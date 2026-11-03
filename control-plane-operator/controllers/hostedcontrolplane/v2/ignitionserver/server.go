package ignitionserver

import (
	"slices"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/support/config"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/podspec"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

const (
	servingCertMountPath           = "/var/run/secrets/ignition/serving-cert"
	tokenVerificationKeyVolumeName = "token-verification-key"
	tokenVerificationKeyMountPath  = "/var/run/secrets/ignition/token-verification-key"
)

// generationVolumeNames are the scratch volumes the monolith uses only while rendering payloads. The
// stateless serving tier never generates, so they (and the fetch-feature-gate init container that
// populates them) are dropped.
var generationVolumeNames = sets.New("payloads", "bootstrap-manifests", "manifests", "shared")

// adaptServingDeployment transforms the shared ignition-server monolith manifest into the stateless
// serving tier: the HyperShift Operator image running the ignition-payload-server subcommand, verifying
// node bearer tokens against the generator's signing key. It deliberately leaves the serving-cert volume
// (and the IBM Cloud swap below) untouched so the reused ignition-server-proxy's backend TLS to
// ignition-server:443 is byte-identical to the monolith's.
func (ign *ignitionServer) adaptServingDeployment(cpContext component.WorkloadContext, deployment *appsv1.Deployment) error {
	hcp := cpContext.HCP
	tlsArgs, err := config.TLSArgs(hcp.Spec.Configuration.GetTLSSecurityProfile())
	if err != nil {
		return err
	}

	// No generation phase: drop the feature-gate init container and the generation scratch volumes.
	deployment.Spec.Template.Spec.InitContainers = nil
	deployment.Spec.Template.Spec.Volumes = slices.DeleteFunc(deployment.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return generationVolumeNames.Has(v.Name)
	})

	// Mount only the public token-verification key (tls.crt) from the generator's signing-key Secret;
	// the private key never reaches the serving tier.
	deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: tokenVerificationKeyVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  TokenSigningKeySecretName,
				DefaultMode: ptr.To[int32](0416),
				Items: []corev1.KeyToPath{
					{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey},
				},
			},
		},
	})

	podspec.UpdateContainer(ComponentName, deployment.Spec.Template.Spec.Containers, func(c *corev1.Container) {
		c.Image = ign.hypershiftOperatorImage
		c.Command = []string{"/usr/bin/hypershift-operator", "ignition-payload-server"}
		c.Args = []string{
			"--cert-file", servingCertMountPath + "/" + corev1.TLSCertKey,
			"--key-file", servingCertMountPath + "/" + corev1.TLSPrivateKeyKey,
			"--token-verification-key-file", tokenVerificationKeyMountPath + "/" + corev1.TLSCertKey,
		}
		c.Args = append(c.Args, tlsArgs...)

		// Drop the generation-only image-override env; MY_NAMESPACE stays.
		c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool {
			return e.Name == "OPENSHIFT_IMG_OVERRIDES"
		})

		c.VolumeMounts = slices.DeleteFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return generationVolumeNames.Has(m.Name)
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      tokenVerificationKeyVolumeName,
			MountPath: tokenVerificationKeyMountPath,
			ReadOnly:  true,
		})
	})

	// Preserve the legacy IBM Cloud serving-cert swap: there the server is exposed directly (no proxy),
	// so it presents the route-host cert instead of the service-name one.
	if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform {
		podspec.UpdateVolume("serving-cert", deployment.Spec.Template.Spec.Volumes, func(v *corev1.Volume) {
			v.Secret.SecretName = ignitionserver.IgnitionServingCertSecret("").Name
		})
	}

	return nil
}

// adaptServerRole narrows the shared ignition-server Role to what the stateless serving tier needs: read
// the Secret-backed PayloadStore and record IgnitionReached on the owning IgnitionPayload. It drops the
// monolith's configmaps/events and hostedcontrolplanes access, which only the generation half uses.
func adaptServerRole(_ component.WorkloadContext, role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{"ignitionpayloads"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{"ignitionpayloads/status"},
			Verbs:     []string{"get", "list", "watch", "update", "patch"},
		},
	}
	return nil
}
