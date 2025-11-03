package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostedAWSCluster defines AWS infrastructure for a hosted cluster
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=hawsc
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Infrastructure ready status"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region",description="AWS Region"
// +kubebuilder:printcolumn:name="Endpoint Access",type="string",JSONPath=".spec.endpointAccess",description="Endpoint access type"
type HostedAWSCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostedAWSClusterSpec   `json:"spec,omitempty"`
	Status HostedAWSClusterStatus `json:"status,omitempty"`
}

// HostedAWSClusterSpec defines the desired state of HostedAWSCluster
type HostedAWSClusterSpec struct {
	// region is the AWS region in which the cluster resides. This configures the
	// OCP control plane cloud integrations, and is used by NodePool to resolve
	// the correct boot AMI for a given release.
	//
	// +immutable
	// +required
	// +kubebuilder:validation:MaxLength=255
	Region string `json:"region"`

	// cloudProviderConfig specifies AWS networking configuration for the control
	// plane.
	// This is mainly used for cloud provider controller config:
	// https://github.com/kubernetes/kubernetes/blob/f5be5052e3d0808abb904aebd3218fe4a5c2dd82/staging/src/k8s.io/legacy-cloud-providers/aws/aws.go#L1347-L1364
	//
	// +optional
	// +immutable
	CloudProviderConfig *AWSCloudProviderConfig `json:"cloudProviderConfig,omitempty"`

	// serviceEndpoints specifies optional custom endpoints which will override
	// the default service endpoint of specific AWS Services.
	//
	// There must be only one ServiceEndpoint for a given service name.
	//
	// +optional
	// +immutable
	// +kubebuilder:validation:MaxItems=50
	ServiceEndpoints []AWSServiceEndpoint `json:"serviceEndpoints,omitempty"`

	// rolesRef contains references to various AWS IAM roles required to enable
	// integrations such as OIDC.
	//
	// +immutable
	// +required
	RolesRef AWSRolesRef `json:"rolesRef"`

	// resourceTags is a list of additional tags to apply to AWS resources created
	// for the cluster. See
	// https://docs.aws.amazon.com/general/latest/gr/aws_tagging.html for
	// information on tagging AWS resources. AWS supports a maximum of 50 tags per
	// resource. OpenShift reserves 25 tags for its use, leaving 25 tags available
	// for the user.
	// Changes to this field will be propagated in-place to AWS resources (VPC Endpoints, EC2 instances, initial EBS volumes and default/endpoint security groups).
	// These tags will be propagated to the infrastructure CR in the guest cluster, where other OCP operators might choose to honor this input to reconcile AWS resources created by them.
	// Please consult the official documentation for a list of all AWS resources that support in-place tag updates.
	// These take precedence over tags defined out of band (i.e., tags added manually or by other tools outside of HyperShift) in AWS in case of conflicts.
	//
	// +kubebuilder:validation:MaxItems=25
	// +optional
	ResourceTags []AWSResourceTag `json:"resourceTags,omitempty"`

	// endpointAccess specifies the publishing scope of cluster endpoints. The
	// default is Public.
	//
	// +kubebuilder:validation:Enum=Public;PublicAndPrivate;Private
	// +kubebuilder:default=Public
	// +optional
	EndpointAccess AWSEndpointAccessType `json:"endpointAccess,omitempty"`

	// additionalAllowedPrincipals specifies a list of additional allowed principal ARNs
	// to be added to the hosted control plane's VPC Endpoint Service to enable additional
	// VPC Endpoint connection requests to be automatically accepted.
	// See https://docs.aws.amazon.com/vpc/latest/privatelink/configure-endpoint-service.html
	// for more details around VPC Endpoint Service allowed principals.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=25
	// +kubebuilder:validation:items:MaxLength=255
	AdditionalAllowedPrincipals []string `json:"additionalAllowedPrincipals,omitempty"`

	// sharedVPC contains fields that must be specified if the HostedCluster must use a VPC that is
	// created in a different AWS account and is shared with the AWS account where the HostedCluster
	// will be created.
	//
	// +optional
	SharedVPC *AWSSharedVPC `json:"sharedVPC,omitempty"`
}

// AWSCloudProviderConfig specifies AWS networking configuration.
type AWSCloudProviderConfig struct {
	// subnet is the subnet to use for control plane cloud resources.
	//
	// +optional
	Subnet *AWSResourceReference `json:"subnet,omitempty"`

	// zone is the availability zone where control plane cloud resources are
	// created.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Zone string `json:"zone,omitempty"`

	// vpc is the VPC to use for control plane cloud resources.
	// +required
	// +kubebuilder:validation:MaxLength=255
	VPC string `json:"vpc"`
}

// AWSResourceReference is a reference to a specific AWS resource by ID or filters.
// Only one of ID or Filters may be specified. Specifying more than one will result in
// a validation error.
type AWSResourceReference struct {
	// id of resource
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ID *string `json:"id,omitempty"`

	// filters is a set of key/value pairs used to identify a resource
	// They are applied according to the rules defined by the AWS API:
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Using_Filtering.html
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Filters []Filter `json:"filters,omitempty"`
}

// Filter is a filter used to identify an AWS resource
type Filter struct {
	// name is the name of the filter.
	// +required
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// values is a list of values for the filter.
	// +required
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=255
	Values []string `json:"values"`
}

// AWSServiceEndpoint stores the configuration for services to
// override existing defaults of AWS Services.
type AWSServiceEndpoint struct {
	// name is the name of the AWS service.
	// This must be provided and cannot be empty.
	// +required
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// url is fully qualified URI with scheme https, that overrides the default generated
	// endpoint for a client.
	// This must be provided and cannot be empty.
	//
	// +required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// AWSRolesRef contains references to various AWS IAM roles required for operators to make calls against the AWS API.
type AWSRolesRef struct {
	// ingressARN is an ARN value referencing a role appropriate for the Ingress Operator.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	IngressARN string `json:"ingressARN"`

	// imageRegistryARN is an ARN value referencing a role appropriate for the Image Registry Operator.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	ImageRegistryARN string `json:"imageRegistryARN"`

	// storageARN is an ARN value referencing a role appropriate for the Storage Operator.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	StorageARN string `json:"storageARN"`

	// networkARN is an ARN value referencing a role appropriate for the Network Operator.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	NetworkARN string `json:"networkARN"`

	// kubeCloudControllerARN is an ARN value referencing a role appropriate for the KCM/KCC.
	// Refer to https://docs.aws.amazon.com/eks/latest/userguide/create-node-role.html
	// for details about the required permissions for the role.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	KubeCloudControllerARN string `json:"kubeCloudControllerARN"`

	// nodePoolManagementARN is an ARN value referencing a role appropriate for the CAPI controllers.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	NodePoolManagementARN string `json:"nodePoolManagementARN"`

	// controlPlaneOperatorARN  is an ARN value referencing a role appropriate for the Control Plane Operator.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	ControlPlaneOperatorARN string `json:"controlPlaneOperatorARN"`
}

// AWSResourceTag is a tag to apply to AWS resources created for the cluster.
type AWSResourceTag struct {
	// key is the key of the tag.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z_.:/=+-@]+$`
	Key string `json:"key"`
	// value is the value of the tag.
	//
	// Some AWS service do not support empty values. Since tags are added to
	// resources in many services, the length of the tag value must meet the
	// requirements of all services.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z_.:/=+-@]+$`
	Value string `json:"value"`
}

// AWSEndpointAccessType specifies the publishing scope of cluster endpoints.
type AWSEndpointAccessType string

const (
	// Public endpoint access allows public API server access and public node
	// communication with the control plane.
	Public AWSEndpointAccessType = "Public"

	// PublicAndPrivate endpoint access allows public API server access and
	// private node communication with the control plane.
	PublicAndPrivate AWSEndpointAccessType = "PublicAndPrivate"

	// Private endpoint access allows only private API server access and private
	// node communication with the control plane.
	Private AWSEndpointAccessType = "Private"
)

// AWSSharedVPC contains fields needed to create a HostedCluster using a VPC that has been
// created and shared from a different AWS account than the AWS account where the cluster
// is getting created.
type AWSSharedVPC struct {
	// rolesRef contains references to roles in the VPC owner account that enable a
	// HostedCluster on a shared VPC.
	//
	// +required
	RolesRef AWSSharedVPCRolesRef `json:"rolesRef"`

	// localZoneID is the ID of the route53 hosted zone for [cluster-name].hypershift.local that is
	// associated with the HostedCluster's VPC and exists in the VPC owner account.
	//
	// +required
	// +kubebuilder:validation:MaxLength=32
	LocalZoneID string `json:"localZoneID"`
}

// AWSSharedVPCRolesRef contains references to roles required for establishing a HostedCluster
// with a VPC in a shared VPC setup.
type AWSSharedVPCRolesRef struct {
	// ingressARN is the ARN of the role in the VPC owner account that will be
	// assumed to create cloud resources for Ingress.
	//
	// +required
	// +kubebuilder:validation:MaxLength=2048
	IngressARN string `json:"ingressARN"`

	// controlPlaneARN is the ARN of the role in the VPC owner account that will
	// be assumed to create cloud resources for the control plane.
	//
	// +required
	// +kubebuilder:validation:MaxLength=2048
	ControlPlaneARN string `json:"controlPlaneARN"`
}

// HostedAWSClusterStatus defines the observed state of HostedAWSCluster
type HostedAWSClusterStatus struct {
	// Ready indicates infrastructure is ready
	Ready bool `json:"ready"`

	// infrastructureCRRef is a reference to the CAPI infrastructure CR (AWSCluster)
	// that was created by this provider controller.
	// This allows the HyperShift operator to generically reference the infrastructure
	// without knowing platform-specific details.
	// +optional
	InfrastructureCRRef *corev1.ObjectReference `json:"infrastructureCRRef,omitempty"`

	// Conditions represent the latest available observations of the HostedAWSCluster's current state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last generation reconciled by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true

// HostedAWSClusterList contains a list of HostedAWSCluster
type HostedAWSClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostedAWSCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostedAWSCluster{}, &HostedAWSClusterList{})
}
