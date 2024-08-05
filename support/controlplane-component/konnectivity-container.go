package controlplanecomponent

import (
	"path"
	"strconv"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	"github.com/openshift/hypershift/support/proxy"
	"github.com/openshift/hypershift/support/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	configv1 "github.com/openshift/api/config/v1"
)

type ProxyMode string

const (
	Socks5 ProxyMode = "Socks5"
	HTTPS  ProxyMode = "HTTPS"
)

type KonnectivityContainerOptions struct {
	Mode ProxyMode
	// defaults to 'kubeconfig'
	KubeconfingVolumeName string

	// socks5 proxy mode args
	ConnectDirectlyToCloudAPIs      bool
	ResolveFromGuestClusterDNS      bool
	ResolveFromManagementClusterDNS bool
	DisableResolver                 bool
}

func (opts *KonnectivityContainerOptions) injectKonnectivityContainer(cpContext ControlPlaneContext, deployment *appsv1.Deployment) error {
	if opts.Mode == "" {
		// programmer error.
		panic("Konnectivity proxy mode must be specified!")
	}

	hcp := cpContext.Hcp
	var proxyAdditionalCAs []corev1.VolumeProjection
	if hcp.Spec.AdditionalTrustBundle != nil {
		proxyAdditionalCAs = append(proxyAdditionalCAs, corev1.VolumeProjection{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: *hcp.Spec.AdditionalTrustBundle,
				Items:                []corev1.KeyToPath{{Key: "ca-bundle.crt", Path: "additional-ca-bundle.pem"}},
			},
		})
	}

	if hcp.Spec.Configuration != nil && hcp.Spec.Configuration.Proxy != nil && len(hcp.Spec.Configuration.Proxy.TrustedCA.Name) > 0 {
		proxyAdditionalCAs = append(proxyAdditionalCAs, corev1.VolumeProjection{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: hcp.Spec.Configuration.Proxy.TrustedCA.Name,
				},
				Items: []corev1.KeyToPath{{Key: "ca-bundle.crt", Path: "proxy-trusted-ca.pem"}},
			},
		})
	}

	image := cpContext.ReleaseImageProvider.GetImage(util.CPOImageName)

	deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, opts.buildContainer(hcp, image, proxyAdditionalCAs))
	deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, opts.buildVolumes(proxyAdditionalCAs)...)
	return nil
}

const certsTrustPath = "/etc/pki/tls/certs"

func (opts *KonnectivityContainerOptions) buildContainer(hcp *hyperv1.HostedControlPlane, image string, proxyAdditionalCAs []corev1.VolumeProjection) corev1.Container {
	var proxyConfig *configv1.ProxySpec
	if hcp.Spec.Configuration != nil {
		proxyConfig = hcp.Spec.Configuration.Proxy
	}

	command := []string{"/usr/bin/control-plane-operator"}
	args := []string{"run"}
	switch opts.Mode {
	case HTTPS:
		command = append(command, "konnectivity-https-proxy")
		if proxyConfig != nil {
			noProxy := proxy.DefaultNoProxy(hcp)
			args = append(args, "--http-proxy", proxyConfig.HTTPProxy)
			args = append(args, "--https-proxy", proxyConfig.HTTPSProxy)
			args = append(args, "--no-proxy", noProxy)
		}
	case Socks5:
		command = append(command, "konnectivity-socks5-proxy")
		args = append(args, "--connect-directly-to-cloud-apis", strconv.FormatBool(opts.ConnectDirectlyToCloudAPIs))
		args = append(args, "--resolve-from-guest-cluster-dns", strconv.FormatBool(opts.ResolveFromGuestClusterDNS))
		args = append(args, "--resolve-from-management-cluster-dns", strconv.FormatBool(opts.ResolveFromManagementClusterDNS))
		args = append(args, "--disable-resolver", strconv.FormatBool(opts.DisableResolver))
	}

	kubeconfingVolumeName := opts.KubeconfingVolumeName
	if kubeconfingVolumeName == "" {
		kubeconfingVolumeName = "kubeconfig"
	}

	container := corev1.Container{
		Name:    "konnectivity-proxy",
		Image:   image,
		Command: command,
		Args:    args,

		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		Env: []corev1.EnvVar{{
			Name:  "KUBECONFIG",
			Value: "/etc/kubernetes/secrets/kubeconfig/kubeconfig",
		}},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      kubeconfingVolumeName,
				MountPath: "/etc/kubernetes/secrets/kubeconfig",
			},
			{
				Name:      "konnectivity-proxy-cert",
				MountPath: "/etc/konnectivity/proxy-client",
			},
			{
				Name:      "konnectivity-proxy-ca",
				MountPath: "/etc/konnectivity/proxy-ca",
			},
		},
	}

	if len(proxyAdditionalCAs) > 0 {
		for _, additionalCA := range proxyAdditionalCAs {
			for _, item := range additionalCA.ConfigMap.Items {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
					Name:      "proxy-additional-trust-bundle",
					MountPath: path.Join(certsTrustPath, item.Path),
					SubPath:   item.Path,
				})
			}
		}
	}

	return container
}

func (opts *KonnectivityContainerOptions) buildVolumes(proxyAdditionalCAs []corev1.VolumeProjection) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "konnectivity-proxy-cert",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  manifests.KonnectivityClientSecret("").Name,
					DefaultMode: ptr.To[int32](0640),
				},
			},
		},
		{
			Name: "konnectivity-proxy-ca",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: manifests.KonnectivityCAConfigMap("").Name,
					},
				},
			},
		},
	}

	if len(proxyAdditionalCAs) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: "proxy-additional-trust-bundle",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources:     proxyAdditionalCAs,
					DefaultMode: ptr.To[int32](420),
				},
			},
		})
	}

	return volumes
}
