package ignitionpayloadcontroller

import (
	"bytes"
	"fmt"

	"github.com/openshift/hypershift/support/api"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/imageregistry"
	"github.com/openshift/hypershift/support/podspec"
	"github.com/openshift/hypershift/support/proxy"

	configv1 "github.com/openshift/api/config/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// adaptDeployment sets the HO image, the release-image-pull args/env (registry overrides,
// platform, OpenShift image overrides, proxy), the feature-gate manifest, and the optional
// additional trust bundle. The generator half mirrors the legacy ignition-server deployment
// adapter, minus the serving/TLS pieces (the controller renders payloads; it does not serve).
func (o *Options) adaptDeployment(cpContext component.WorkloadContext, deployment *appsv1.Deployment) error {
	hcp := cpContext.HCP

	if hcp.Spec.Configuration != nil && hcp.Spec.Configuration.FeatureGate != nil {
		featureGate := &configv1.FeatureGate{
			TypeMeta: metav1.TypeMeta{
				Kind:       "FeatureGate",
				APIVersion: configv1.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster",
			},
			Spec: *hcp.Spec.Configuration.FeatureGate,
		}

		featureGateBuffer := &bytes.Buffer{}
		if err := api.YamlSerializer.Encode(featureGate, featureGateBuffer); err != nil {
			return fmt.Errorf("failed to encode feature gates: %w", err)
		}

		podspec.UpdateContainer("fetch-feature-gate", deployment.Spec.Template.Spec.InitContainers, func(c *corev1.Container) {
			podspec.UpsertEnvVar(c, corev1.EnvVar{
				Name:  "FEATURE_GATE_YAML",
				Value: featureGateBuffer.String(),
			})
		})
	}

	podspec.UpdateContainer(ComponentName, deployment.Spec.Template.Spec.Containers, func(c *corev1.Container) {
		c.Image = o.HyperShiftOperatorImage
		c.Args = append(c.Args,
			"--registry-overrides", imageregistry.ConvertRegistryOverridesToCommandLineFlag(o.ReleaseProvider.GetRegistryOverrides()),
			"--platform", string(hcp.Spec.Platform.Type),
		)
		podspec.UpsertEnvVar(c, corev1.EnvVar{
			Name:  "OPENSHIFT_IMG_OVERRIDES",
			Value: imageregistry.ConvertOpenShiftImageRegistryOverridesToCommandLineFlag(o.ReleaseProvider.GetOpenShiftImageRegistryOverrides()),
		})
		proxy.SetEnvVars(&c.Env)
	})

	if hcp.Spec.AdditionalTrustBundle != nil {
		podspec.DeploymentAddTrustBundleVolume(hcp.Spec.AdditionalTrustBundle, deployment)
	}

	return nil
}
