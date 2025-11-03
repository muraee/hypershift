package aws

import (
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	"github.com/openshift/hypershift/cmd/cluster/core"
	awsutil "github.com/openshift/hypershift/cmd/util"

	configv1 "github.com/openshift/api/config/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// HostedAWSClusterAPIVersion is the API version for HostedAWSCluster
	HostedAWSClusterAPIVersion = "infrastructure.hypershift.openshift.io/v1beta1"
	// HostedAWSClusterKind is the kind for HostedAWSCluster
	HostedAWSClusterKind = "HostedAWSCluster"
)

// GenerateHostedAWSCluster generates a HostedAWSCluster CRD from the create options.
// This is used when creating a cluster with the new provider-based architecture.
// Since providers/aws is a separate module, we use unstructured to avoid import issues.
func (o *CreateOptions) GenerateHostedAWSCluster(clusterName string) (*unstructured.Unstructured, error) {
	// Parse resource tags
	tagMap, err := awsutil.ParseAWSTags(o.AdditionalTags)
	if err != nil {
		return nil, fmt.Errorf("failed to parse additional tags: %w", err)
	}
	var tags []interface{}
	for k, v := range tagMap {
		tags = append(tags, map[string]interface{}{
			"key":   k,
			"value": v,
		})
	}

	// Build the spec
	spec := map[string]interface{}{
		"region": o.Region,
		"cloudProviderConfig": map[string]interface{}{
			"vpc": o.infra.VPCID,
			"subnet": map[string]interface{}{
				"id": o.infra.Zones[0].SubnetID,
			},
			"zone": o.infra.Zones[0].Name,
		},
		"rolesRef": map[string]interface{}{
			"ingressARN":              o.iamInfo.Roles.IngressARN,
			"imageRegistryARN":        o.iamInfo.Roles.ImageRegistryARN,
			"storageARN":              o.iamInfo.Roles.StorageARN,
			"networkARN":              o.iamInfo.Roles.NetworkARN,
			"kubeCloudControllerARN":  o.iamInfo.Roles.KubeCloudControllerARN,
			"nodePoolManagementARN":   o.iamInfo.Roles.NodePoolManagementARN,
			"controlPlaneOperatorARN": o.iamInfo.Roles.ControlPlaneOperatorARN,
		},
		"endpointAccess": o.EndpointAccess,
	}

	// Add resource tags if present
	if len(tags) > 0 {
		spec["resourceTags"] = tags
	}

	// Add SharedVPC configuration if present
	if o.iamInfo.SharedIngressRoleARN != "" && o.iamInfo.SharedControlPlaneRoleARN != "" {
		spec["sharedVPC"] = map[string]interface{}{
			"rolesRef": map[string]interface{}{
				"ingressARN":      o.iamInfo.SharedIngressRoleARN,
				"controlPlaneARN": o.iamInfo.SharedControlPlaneRoleARN,
			},
			"localZoneID": o.infra.LocalZoneID,
		}

		// Add the shared control plane role to additional allowed principals
		spec["additionalAllowedPrincipals"] = []string{o.iamInfo.SharedControlPlaneRoleARN}
	}

	// Create the unstructured object
	hostedAWSCluster := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": HostedAWSClusterAPIVersion,
			"kind":       HostedAWSClusterKind,
			"metadata": map[string]interface{}{
				"name":      fmt.Sprintf("%s-infra", clusterName),
				"namespace": o.namespace,
			},
			"spec": spec,
		},
	}

	return hostedAWSCluster, nil
}

// ApplyPlatformSpecificsForProvider applies platform-specific configuration
// when using the provider-based architecture. This is a simplified version
// that only sets the infrastructureRef and does not populate the embedded
// platform spec.
func (o *CreateOptions) ApplyPlatformSpecificsForProvider(cluster client.Object, hostedAWSClusterName string) error {
	hcluster, ok := cluster.(*hyperv1.HostedCluster)
	if !ok {
		return fmt.Errorf("expected HostedCluster, got %T", cluster)
	}

	// Set basic infrastructure metadata
	hcluster.Spec.InfraID = o.infra.InfraID
	hcluster.Spec.IssuerURL = o.iamInfo.IssuerURL

	if len(o.ServiceAccountTokenIssuerKeyPath) > 0 {
		hcluster.Spec.ServiceAccountSigningKey = &corev1.LocalObjectReference{
			Name: serviceAccountTokenIssuerSecret(o.namespace, o.name).Name,
		}
	}

	// Set DNS configuration
	var baseDomainPrefix *string
	if o.infra.BaseDomainPrefix == "none" {
		baseDomainPrefix = ptr.To("")
	} else if o.infra.BaseDomainPrefix != "" {
		baseDomainPrefix = ptr.To(o.infra.BaseDomainPrefix)
	}
	hcluster.Spec.DNS = hyperv1.DNSSpec{
		BaseDomain:       o.infra.BaseDomain,
		BaseDomainPrefix: baseDomainPrefix,
		PublicZoneID:     o.infra.PublicZoneID,
		PrivateZoneID:    o.infra.PrivateZoneID,
	}

	// Set platform with infrastructureRef instead of embedded spec
	hcluster.Spec.Platform = hyperv1.PlatformSpec{
		Type: hyperv1.AWSPlatform,
	}

	// Set the infrastructure reference to the HostedAWSCluster
	hcluster.Spec.InfrastructureRef = &corev1.ObjectReference{
		APIVersion: HostedAWSClusterAPIVersion,
		Kind:       HostedAWSClusterKind,
		Name:       hostedAWSClusterName,
		Namespace:  o.namespace,
	}

	// Configure AutoNode if enabled
	if o.AutoNode {
		hcluster.Spec.AutoNode = &hyperv1.AutoNode{
			Provisioner: &hyperv1.ProvisionerConfig{
				Name: hyperv1.ProvisionerKarpenter,
				Karpenter: &hyperv1.KarpenterConfig{
					Platform: hyperv1.AWSPlatform,
					AWS: &hyperv1.KarpenterAWSConfig{
						RoleARN: o.iamInfo.KarpenterRoleARN,
					},
				},
			},
		}
	}

	// Configure MachineNetwork if present
	if o.infra.MachineCIDR != "" {
		cidr, err := ipnet.ParseCIDR(o.infra.MachineCIDR)
		if err != nil {
			return fmt.Errorf("parsing MachineCIDR (%s): %w", o.infra.MachineCIDR, err)
		}
		hcluster.Spec.Networking.MachineNetwork = []hyperv1.MachineNetworkEntry{{CIDR: *cidr}}
	}

	// Configure proxy settings
	switch {
	case o.infra.SecureProxyAddr != "":
		hcluster.Spec.Configuration.Proxy = &configv1.ProxySpec{
			HTTPProxy:  o.infra.ProxyAddr,
			HTTPSProxy: o.infra.SecureProxyAddr,
		}
	case o.infra.ProxyAddr != "":
		hcluster.Spec.Configuration.Proxy = &configv1.ProxySpec{
			HTTPProxy:  o.infra.ProxyAddr,
			HTTPSProxy: o.infra.ProxyAddr,
		}
	}
	if hcluster.Spec.Configuration.Proxy != nil && o.infra.ProxyCA != "" {
		hcluster.Spec.Configuration.Proxy.TrustedCA.Name = o.proxyCAConfigMapName()
	}

	// Configure KMS secret encryption if present
	if len(o.iamInfo.KMSProviderRoleARN) > 0 {
		hcluster.Spec.SecretEncryption = &hyperv1.SecretEncryptionSpec{
			Type: hyperv1.KMS,
			KMS: &hyperv1.KMSSpec{
				Provider: hyperv1.AWS,
				AWS: &hyperv1.AWSKMSSpec{
					Region: o.Region,
					ActiveKey: hyperv1.AWSKMSKeyEntry{
						ARN: o.iamInfo.KMSKeyARN,
					},
					Auth: hyperv1.AWSKMSAuthSpec{
						AWSKMSRoleARN: o.iamInfo.KMSProviderRoleARN,
					},
				},
			},
		}
	}

	// Configure ingress service publishing strategy
	hcluster.Spec.Services = core.GetIngressServicePublishingStrategyMapping(hcluster.Spec.Networking.NetworkType, o.externalDNSDomain != "")
	if o.externalDNSDomain != "" {
		endpointAccess := hyperv1.AWSEndpointAccessType(o.EndpointAccess)
		for i, svc := range hcluster.Spec.Services {
			switch svc.Service {
			case hyperv1.APIServer:
				hcluster.Spec.Services[i].Route = &hyperv1.RoutePublishingStrategy{
					Hostname: fmt.Sprintf("api-%s.%s", hcluster.Name, o.externalDNSDomain),
				}

			case hyperv1.OAuthServer:
				hcluster.Spec.Services[i].Route = &hyperv1.RoutePublishingStrategy{
					Hostname: fmt.Sprintf("oauth-%s.%s", hcluster.Name, o.externalDNSDomain),
				}

			case hyperv1.Konnectivity:
				if endpointAccess == hyperv1.Public {
					hcluster.Spec.Services[i].Route = &hyperv1.RoutePublishingStrategy{
						Hostname: fmt.Sprintf("konnectivity-%s.%s", hcluster.Name, o.externalDNSDomain),
					}
				}

			case hyperv1.Ignition:
				if endpointAccess == hyperv1.Public {
					hcluster.Spec.Services[i].Route = &hyperv1.RoutePublishingStrategy{
						Hostname: fmt.Sprintf("ignition-%s.%s", hcluster.Name, o.externalDNSDomain),
					}
				}
			}
		}
	}

	// Set public IPs annotation if needed
	if o.infra.PublicOnly {
		if hcluster.Annotations == nil {
			hcluster.Annotations = map[string]string{}
		}
		hcluster.Annotations[hyperv1.AWSMachinePublicIPs] = "true"
	}

	return nil
}
