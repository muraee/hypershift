package ignitionserver

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/assets"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	component "github.com/openshift/hypershift/support/controlplane-component"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdaptServingDeployment(t *testing.T) {
	loadServing := func(t *testing.T, hcp *hyperv1.HostedControlPlane) *corev1.Container {
		t.Helper()
		g := NewWithT(t)

		deployment, err := assets.LoadDeploymentManifest(ComponentName)
		g.Expect(err).ToNot(HaveOccurred())

		ign := &ignitionServer{newModel: true, hypershiftOperatorImage: testHOImage}
		g.Expect(ign.adaptServingDeployment(component.WorkloadContext{Context: t.Context(), HCP: hcp}, deployment)).To(Succeed())

		// No generation phase.
		g.Expect(deployment.Spec.Template.Spec.InitContainers).To(BeEmpty())
		for _, v := range deployment.Spec.Template.Spec.Volumes {
			g.Expect(generationVolumeNames.Has(v.Name)).To(BeFalse(), "generation volume %q should be removed", v.Name)
		}

		var container *corev1.Container
		for i := range deployment.Spec.Template.Spec.Containers {
			if deployment.Spec.Template.Spec.Containers[i].Name == ComponentName {
				container = &deployment.Spec.Template.Spec.Containers[i]
			}
		}
		g.Expect(container).ToNot(BeNil())
		return container
	}

	t.Run("runs the HO serving subcommand and verifies tokens", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "hcp", Namespace: "ns"}}
		container := loadServing(t, hcp)

		g.Expect(container.Image).To(Equal(testHOImage))
		g.Expect(container.Command).To(Equal([]string{"/usr/bin/hypershift-operator", "ignition-payload-server"}))
		g.Expect(container.Args).To(ContainElements(
			"--cert-file", servingCertMountPath+"/tls.crt",
			"--key-file", servingCertMountPath+"/tls.key",
			"--token-verification-key-file", tokenVerificationKeyMountPath+"/tls.crt",
		))
		g.Expect(container.Args).ToNot(ContainElement(ContainSubstring("feature-gate-manifest")))

		// The serving cert mount is preserved (the reused proxy backend TLS is unchanged); the
		// token-verification key is added; generation mounts are gone.
		mountNames := map[string]bool{}
		for _, m := range container.VolumeMounts {
			mountNames[m.Name] = true
			g.Expect(generationVolumeNames.Has(m.Name)).To(BeFalse())
		}
		g.Expect(mountNames).To(HaveKey("serving-cert"))
		g.Expect(mountNames).To(HaveKey(tokenVerificationKeyVolumeName))

		var envNames []string
		for _, e := range container.Env {
			envNames = append(envNames, e.Name)
		}
		g.Expect(envNames).To(ContainElement("MY_NAMESPACE"))
		g.Expect(envNames).ToNot(ContainElement("OPENSHIFT_IMG_OVERRIDES"))

		var servingPort bool
		for _, p := range container.Ports {
			if p.ContainerPort == 9090 {
				servingPort = true
			}
		}
		g.Expect(servingPort).To(BeTrue(), "serving port 9090 should be preserved")
	})

	t.Run("mounts only the public token-verification key from the signing-key Secret", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "hcp", Namespace: "ns"}}

		deployment, err := assets.LoadDeploymentManifest(ComponentName)
		g.Expect(err).ToNot(HaveOccurred())
		ign := &ignitionServer{newModel: true, hypershiftOperatorImage: testHOImage}
		g.Expect(ign.adaptServingDeployment(component.WorkloadContext{Context: t.Context(), HCP: hcp}, deployment)).To(Succeed())

		var vol *corev1.Volume
		for i := range deployment.Spec.Template.Spec.Volumes {
			if deployment.Spec.Template.Spec.Volumes[i].Name == tokenVerificationKeyVolumeName {
				vol = &deployment.Spec.Template.Spec.Volumes[i]
			}
		}
		g.Expect(vol).ToNot(BeNil())
		g.Expect(vol.Secret).ToNot(BeNil())
		g.Expect(vol.Secret.SecretName).To(Equal(TokenSigningKeySecretName))
		g.Expect(vol.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}}))
	})

	t.Run("keeps the service-name serving cert for non-IBM platforms", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "hcp", Namespace: "ns"},
			Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
		}
		deployment, err := assets.LoadDeploymentManifest(ComponentName)
		g.Expect(err).ToNot(HaveOccurred())
		ign := &ignitionServer{newModel: true, hypershiftOperatorImage: testHOImage}
		g.Expect(ign.adaptServingDeployment(component.WorkloadContext{Context: t.Context(), HCP: hcp}, deployment)).To(Succeed())
		for _, v := range deployment.Spec.Template.Spec.Volumes {
			if v.Name == "serving-cert" {
				g.Expect(v.Secret.SecretName).To(Equal("ignition-server"))
			}
		}
	})

	t.Run("swaps to the route-host serving cert on IBM Cloud", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "hcp", Namespace: "ns"},
			Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}},
		}
		deployment, err := assets.LoadDeploymentManifest(ComponentName)
		g.Expect(err).ToNot(HaveOccurred())
		ign := &ignitionServer{newModel: true, hypershiftOperatorImage: testHOImage}
		g.Expect(ign.adaptServingDeployment(component.WorkloadContext{Context: t.Context(), HCP: hcp}, deployment)).To(Succeed())

		for _, v := range deployment.Spec.Template.Spec.Volumes {
			if v.Name == "serving-cert" {
				g.Expect(v.Secret.SecretName).To(Equal(ignitionserver.IgnitionServingCertSecret("").Name))
			}
		}
	})
}

func TestAdaptServerRole(t *testing.T) {
	g := NewWithT(t)
	role := &rbacv1.Role{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"*"}}}}

	g.Expect(adaptServerRole(component.WorkloadContext{Context: t.Context()}, role)).To(Succeed())

	// The monolith's configmaps/hostedcontrolplanes access is dropped.
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			g.Expect(res).ToNot(Equal("configmaps"))
			g.Expect(res).ToNot(Equal("hostedcontrolplanes"))
		}
	}
	g.Expect(role.Rules).To(ContainElement(rbacv1.PolicyRule{
		APIGroups: []string{""},
		Resources: []string{"secrets"},
		Verbs:     []string{"get", "list", "watch"},
	}))
	g.Expect(role.Rules).To(ContainElement(rbacv1.PolicyRule{
		APIGroups: []string{hyperv1.GroupVersion.Group},
		Resources: []string{"ignitionpayloads/status"},
		Verbs:     []string{"get", "list", "watch", "update", "patch"},
	}))
}
