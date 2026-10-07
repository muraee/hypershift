package ignitionpayloadserver

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/config"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/podspec"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// adaptDeployment sets the HO image and the TLS args. The serving tier has no generator scaffolding
// (no /payloads, no feature-gate init container, no registry overrides). It presents the internal
// server cert on 9090 behind the proxy; on IBMCloud there is no proxy, so the server presents the
// node-facing serving cert directly.
func (o *Options) adaptDeployment(cpContext component.WorkloadContext, deployment *appsv1.Deployment) error {
	hcp := cpContext.HCP
	tlsArgs, err := config.TLSArgs(hcp.Spec.Configuration.GetTLSSecurityProfile())
	if err != nil {
		return err
	}

	podspec.UpdateContainer(ComponentName, deployment.Spec.Template.Spec.Containers, func(c *corev1.Container) {
		c.Image = o.HyperShiftOperatorImage
		if len(tlsArgs) > 0 {
			c.Args = append(c.Args, tlsArgs...)
		}
	})

	if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform {
		podspec.UpdateVolume("serving-cert", deployment.Spec.Template.Spec.Volumes, func(v *corev1.Volume) {
			v.Secret.SecretName = servingCertSecretName
		})
	}

	return nil
}
