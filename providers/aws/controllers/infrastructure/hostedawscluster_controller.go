package infrastructure

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	infrav1 "github.com/openshift/hypershift/providers/aws/api/v1beta1"
	capiaws "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	finalizer = "infrastructure.hypershift.openshift.io/finalizer"

	// Labels used to track ownership across namespaces (since we can't use owner references)
	labelHostedAWSClusterNamespace = "hypershift.openshift.io/hosted-aws-cluster-namespace"
	labelHostedAWSClusterName      = "hypershift.openshift.io/hosted-aws-cluster-name"

	// awsCredentialsTemplate is the template for AWS web identity credentials
	awsCredentialsTemplate = `[default]
role_arn = %s
web_identity_token_file = /var/run/secrets/openshift/serviceaccount/token
sts_regional_endpoints = regional
region = %s
`
)

// HostedAWSClusterReconciler reconciles a HostedAWSCluster object
type HostedAWSClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager
func (r *HostedAWSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Note: We don't use .Owns() for AWSCluster and Secrets because they are created
	// in a different namespace (control plane namespace) and Kubernetes doesn't support
	// cross-namespace owner references. Cleanup is handled manually in reconcileDelete.
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HostedAWSCluster{}).
		Watches(
			&capiaws.AWSCluster{},
			handler.EnqueueRequestsFromMapFunc(r.awsClusterToHostedAWSCluster),
		).
		Watches(
			&hyperv1.HostedCluster{},
			handler.EnqueueRequestsFromMapFunc(r.hostedClusterToHostedAWSCluster),
		).
		Complete(r)
}

// awsClusterToHostedAWSCluster maps an AWSCluster to its owning HostedAWSCluster
// by reading the labels we set on the AWSCluster
func (r *HostedAWSClusterReconciler) awsClusterToHostedAWSCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	awsCluster, ok := obj.(*capiaws.AWSCluster)
	if !ok {
		return nil
	}

	// Read the labels that identify the owning HostedAWSCluster
	namespace, hasNamespace := awsCluster.Labels[labelHostedAWSClusterNamespace]
	name, hasName := awsCluster.Labels[labelHostedAWSClusterName]
	if !hasNamespace || !hasName {
		// AWSCluster doesn't have our labels, not managed by us
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      name,
			},
		},
	}
}

// hostedClusterToHostedAWSCluster maps a HostedCluster to its owned HostedAWSCluster
// by reading the InfrastructureRef in the HostedCluster spec
func (r *HostedAWSClusterReconciler) hostedClusterToHostedAWSCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	hostedCluster, ok := obj.(*hyperv1.HostedCluster)
	if !ok {
		return nil
	}

	// Check if the HostedCluster has an InfrastructureRef pointing to a HostedAWSCluster
	if hostedCluster.Spec.InfrastructureRef == nil {
		return nil
	}

	infraRef := hostedCluster.Spec.InfrastructureRef
	// Verify this is a HostedAWSCluster reference
	if infraRef.APIVersion != infrav1.GroupVersion.String() || infraRef.Kind != "HostedAWSCluster" {
		return nil
	}

	// Return a reconcile request for the referenced HostedAWSCluster
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: infraRef.Namespace,
				Name:      infraRef.Name,
			},
		},
	}
}

// +kubebuilder:rbac:groups=infrastructure.hypershift.openshift.io,resources=hostedawsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.hypershift.openshift.io,resources=hostedawsclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.hypershift.openshift.io,resources=hostedawsclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedcontrolplanes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation loop for HostedAWSCluster
func (r *HostedAWSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the HostedAWSCluster
	hostedAWSCluster := &infrav1.HostedAWSCluster{}
	if err := r.Get(ctx, req.NamespacedName, hostedAWSCluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("HostedAWSCluster not found, ignoring")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !hostedAWSCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, hostedAWSCluster)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(hostedAWSCluster, finalizer) {
		controllerutil.AddFinalizer(hostedAWSCluster, finalizer)
		if err := r.Update(ctx, hostedAWSCluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	// Find the owner HostedCluster
	hostedCluster, err := r.getOwnerHostedCluster(ctx, hostedAWSCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner HostedCluster: %w", err)
	}

	if !hostedCluster.DeletionTimestamp.IsZero() {
		log.Info("HostedCluster is being deleted, skipping reconciliation", "name", hostedCluster.Name)
		return ctrl.Result{}, nil
	}

	hcpNamespace := controlPlaneNamespace(hostedCluster)

	// Reconcile the CAPI AWSCluster
	if err := r.reconcileAWSCluster(ctx, hostedAWSCluster, hostedCluster, hcpNamespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile AWSCluster: %w", err)
	}

	// Reconcile AWS credentials secrets in control plane namespace
	if err := r.reconcileCredentials(ctx, hostedAWSCluster, hostedCluster, hcpNamespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile credentials: %w", err)
	}

	// Populate HCP.Spec.Platform.AWS with CPO-specific configuration using Server-Side Apply
	// This allows the CPO to access platform-specific configuration without importing provider types
	if err := r.reconcileHostedControlPlanePlatform(ctx, hostedAWSCluster, hostedCluster, hcpNamespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile HCP platform configuration: %w", err)
	}

	// Update status
	if err := r.updateStatus(ctx, hostedAWSCluster, hostedCluster, hcpNamespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	return ctrl.Result{}, nil
}

// getOwnerHostedCluster finds the HostedCluster that owns this HostedAWSCluster
func (r *HostedAWSClusterReconciler) getOwnerHostedCluster(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster) (*hyperv1.HostedCluster, error) {
	// Look for owner reference
	for _, ownerRef := range hostedAWSCluster.GetOwnerReferences() {
		if ownerRef.Kind == "HostedCluster" && ownerRef.APIVersion == hyperv1.GroupVersion.String() {
			hostedCluster := &hyperv1.HostedCluster{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: hostedAWSCluster.Namespace, Name: ownerRef.Name}, hostedCluster); err != nil {
				return nil, err
			}
			return hostedCluster, nil
		}
	}

	return nil, fmt.Errorf("waiting for HostedCluster owner reference to be set")
}

// reconcileAWSCluster creates or updates the CAPI AWSCluster based on the existing pattern
func (r *HostedAWSClusterReconciler) reconcileAWSCluster(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster, hostedCluster *hyperv1.HostedCluster, hcpNamespace string) error {
	log := log.FromContext(ctx)

	// CAPI AWSCluster is created in the control plane namespace
	awsCluster := &capiaws.AWSCluster{}
	awsCluster.Name = hostedCluster.Name
	awsCluster.Namespace = hcpNamespace

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, awsCluster, func() error {
		return r.reconcileAWSClusterSpec(awsCluster, hostedAWSCluster, hostedCluster)
	})

	if err != nil {
		return fmt.Errorf("failed to reconcile AWSCluster: %w", err)
	}

	log.Info("Reconciled AWSCluster", "result", result)
	return nil
}

// reconcileAWSClusterSpec configures the AWSCluster spec based on the existing reconcileAWSCluster function
func (r *HostedAWSClusterReconciler) reconcileAWSClusterSpec(awsCluster *capiaws.AWSCluster, hostedAWSCluster *infrav1.HostedAWSCluster, hostedCluster *hyperv1.HostedCluster) error {
	// Set managed-by annotation to indicate external management
	if awsCluster.Annotations == nil {
		awsCluster.Annotations = make(map[string]string)
	}
	awsCluster.Annotations[capiv1.ManagedByAnnotation] = "external"

	// Add labels to track the owning HostedAWSCluster (can't use owner reference cross-namespace)
	if awsCluster.Labels == nil {
		awsCluster.Labels = make(map[string]string)
	}
	awsCluster.Labels[labelHostedAWSClusterNamespace] = hostedAWSCluster.Namespace
	awsCluster.Labels[labelHostedAWSClusterName] = hostedAWSCluster.Name

	// Configure region
	awsCluster.Spec.Region = hostedAWSCluster.Spec.Region

	// Configure VPC if specified
	if hostedAWSCluster.Spec.CloudProviderConfig != nil {
		awsCluster.Spec.NetworkSpec.VPC.ID = hostedAWSCluster.Spec.CloudProviderConfig.VPC
	}

	// Configure resource tags
	if awsCluster.Spec.AdditionalTags == nil {
		awsCluster.Spec.AdditionalTags = capiaws.Tags{}
	}
	for _, entry := range hostedAWSCluster.Spec.ResourceTags {
		awsCluster.Spec.AdditionalTags[entry.Key] = entry.Value
	}

	// Always ensure the kubernetes.io/cluster/<infraID> tag is set to "owned"
	// This is required for the AWS cloud provider to identify cluster resources
	if hostedCluster.Spec.InfraID != "" {
		clusterTag := "kubernetes.io/cluster/" + hostedCluster.Spec.InfraID
		awsCluster.Spec.AdditionalTags[clusterTag] = "owned"
	}

	// CRITICAL: Set control plane endpoint from HostedCluster status
	// This is populated by the CPO controller and propagated to HostedCluster
	if hostedCluster.Status.ControlPlaneEndpoint.Host != "" {
		awsCluster.Spec.ControlPlaneEndpoint = capiv1.APIEndpoint{
			Host: hostedCluster.Status.ControlPlaneEndpoint.Host,
			Port: hostedCluster.Status.ControlPlaneEndpoint.Port,
		}
	}

	// Set status ready (following existing pattern)
	awsCluster.Status.Ready = true

	return nil
}

// reconcileDelete handles cleanup when HostedAWSCluster is deleted
func (r *HostedAWSClusterReconciler) reconcileDelete(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get the owner HostedCluster to determine control plane namespace
	hostedCluster, err := r.getOwnerHostedCluster(ctx, hostedAWSCluster)
	if err != nil {
		// If HostedCluster is not found, it may have been deleted already
		// Proceed with finalizer removal
		log.Info("Owner HostedCluster not found during deletion, proceeding with cleanup", "error", err)
	}

	if hostedCluster != nil {
		hcpNamespace := controlPlaneNamespace(hostedCluster)

		// Note: We don't delete the AWSCluster here because it's owned by the CAPI Cluster resource.
		// The CAPI Cluster will handle cleanup of the AWSCluster via its owner reference.

		// Delete credential secrets that we created (only if labels match)
		secretsToDelete := []*corev1.Secret{
			kubeCloudControllerCredsSecret(hcpNamespace),
			nodePoolManagementCredsSecret(hcpNamespace),
			controlPlaneOperatorCredsSecret(hcpNamespace),
			cloudNetworkConfigControllerCredsSecret(hcpNamespace),
			awsEBSCSIDriverCredsSecret(hcpNamespace),
			awsKMSCredsSecret(hcpNamespace),
		}

		for _, secret := range secretsToDelete {
			// Fetch the secret first to check if it has our labels
			existingSecret := &corev1.Secret{}
			err := r.Get(ctx, client.ObjectKey{
				Namespace: secret.Namespace,
				Name:      secret.Name,
			}, existingSecret)

			if apierrors.IsNotFound(err) {
				// Secret doesn't exist, nothing to do
				continue
			}
			if err != nil {
				log.Error(err, "Failed to get secret for deletion check", "namespace", secret.Namespace, "name", secret.Name)
				continue
			}

			// Verify the secret has our labels before deleting
			if existingSecret.Labels != nil &&
				existingSecret.Labels[labelHostedAWSClusterNamespace] == hostedAWSCluster.Namespace &&
				existingSecret.Labels[labelHostedAWSClusterName] == hostedAWSCluster.Name {

				if err := r.Delete(ctx, existingSecret); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "Failed to delete secret", "namespace", secret.Namespace, "name", secret.Name)
					// Continue deleting other secrets even if one fails
				} else {
					log.Info("Deleted secret", "namespace", secret.Namespace, "name", secret.Name)
				}
			} else {
				log.Info("Skipping secret deletion - labels don't match or are missing",
					"namespace", secret.Namespace,
					"name", secret.Name,
					"expectedNamespace", hostedAWSCluster.Namespace,
					"expectedName", hostedAWSCluster.Name)
			}
		}
	}

	// Remove finalizer
	if controllerutil.ContainsFinalizer(hostedAWSCluster, finalizer) {
		controllerutil.RemoveFinalizer(hostedAWSCluster, finalizer)
		if err := r.Update(ctx, hostedAWSCluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	log.Info("HostedAWSCluster deleted")
	return ctrl.Result{}, nil
}

// updateStatus updates the HostedAWSCluster status based on CAPI AWSCluster status
func (r *HostedAWSClusterReconciler) updateStatus(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster, hostedCluster *hyperv1.HostedCluster, hcpNamespace string) error {
	// Get the corresponding AWSCluster
	awsCluster := &capiaws.AWSCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: hcpNamespace,
		Name:      hostedCluster.Name,
	}, awsCluster); err != nil {
		if apierrors.IsNotFound(err) {
			// AWSCluster not created yet
			hostedAWSCluster.Status.Ready = false
			return r.Status().Update(ctx, hostedAWSCluster)
		}
		return fmt.Errorf("failed to get AWSCluster: %w", err)
	}

	hostedAWSCluster.Status.Ready = true
	hostedAWSCluster.Status.ObservedGeneration = hostedAWSCluster.Generation

	// Set the infrastructure CR reference so HyperShift operator can find it generically
	hostedAWSCluster.Status.InfrastructureCRRef = &corev1.ObjectReference{
		APIVersion: awsCluster.APIVersion,
		Kind:       awsCluster.Kind,
		Namespace:  awsCluster.Namespace,
		Name:       awsCluster.Name,
	}

	return r.Status().Update(ctx, hostedAWSCluster)
}

// reconcileHostedControlPlanePlatform populates HCP.Spec.Platform.AWS using Server-Side Apply
// This allows the CPO to access platform-specific configuration without importing provider types
// In provider mode, the platform provider controller manages the HCP platform spec, not the HostedCluster controller
func (r *HostedAWSClusterReconciler) reconcileHostedControlPlanePlatform(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster, hostedCluster *hyperv1.HostedCluster, hcpNamespace string) error {
	log := log.FromContext(ctx)

	hcpName := hostedCluster.Name
	// Check if HCP exists first
	existingHCP := &hyperv1.HostedControlPlane{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: hcpNamespace,
		Name:      hcpName,
	}, existingHCP); err != nil {
		if apierrors.IsNotFound(err) {
			// HCP not created yet, requeue and wait
			log.Info("HostedControlPlane not found yet, waiting", "namespace", hcpNamespace, "name", hcpName)
			return fmt.Errorf("HostedControlPlane %s/%s not found yet, will requeue", hcpNamespace, hcpName)
		}
		return fmt.Errorf("failed to get HostedControlPlane: %w", err)
	}

	// Build the AWS platform spec
	awsPlatformSpec := buildAWSPlatformSpec(hostedAWSCluster)

	// Convert to unstructured to have precise control over which fields we're managing
	awsPlatformUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(awsPlatformSpec)
	if err != nil {
		return fmt.Errorf("failed to convert AWS platform spec to unstructured: %w", err)
	}

	// Build an unstructured patch with ONLY the spec.platform fields we want to manage
	// This avoids setting zero values for other fields in the HCP spec
	hcpPatch := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": hyperv1.GroupVersion.String(),
			"kind":       "HostedControlPlane",
			"metadata": map[string]interface{}{
				"name":      hcpName,
				"namespace": hcpNamespace,
			},
			"spec": map[string]interface{}{
				"platform": map[string]interface{}{
					"type": string(hyperv1.AWSPlatform),
					"aws":  awsPlatformUnstructured,
				},
			},
		},
	}

	// Apply the patch using Server-Side Apply with field manager "hypershift-aws-provider"
	// This ensures we only own the spec.platform.type and spec.platform.aws fields we set
	if err := r.Patch(ctx, hcpPatch, client.Apply, client.ForceOwnership, client.FieldOwner("hypershift-aws-provider")); err != nil {
		return fmt.Errorf("failed to apply HostedControlPlane platform config: %w", err)
	}

	log.Info("Applied HostedControlPlane platform configuration using SSA",
		"namespace", hcpNamespace,
		"name", hcpName,
		"region", awsPlatformSpec.Region,
		"fieldManager", "hypershift-aws-provider")

	return nil
}

// buildAWSPlatformSpec constructs the AWS platform spec from HostedAWSCluster
func buildAWSPlatformSpec(hostedAWSCluster *infrav1.HostedAWSCluster) *hyperv1.AWSPlatformSpec {
	awsSpec := &hyperv1.AWSPlatformSpec{
		Region: hostedAWSCluster.Spec.Region,
	}

	// Set cloud provider config
	if hostedAWSCluster.Spec.CloudProviderConfig != nil {
		awsSpec.CloudProviderConfig = &hyperv1.AWSCloudProviderConfig{
			VPC:  hostedAWSCluster.Spec.CloudProviderConfig.VPC,
			Zone: hostedAWSCluster.Spec.CloudProviderConfig.Zone,
		}
		if hostedAWSCluster.Spec.CloudProviderConfig.Subnet != nil && hostedAWSCluster.Spec.CloudProviderConfig.Subnet.ID != nil {
			awsSpec.CloudProviderConfig.Subnet = &hyperv1.AWSResourceReference{
				ID: hostedAWSCluster.Spec.CloudProviderConfig.Subnet.ID,
			}
		}
	}

	// Set service endpoints
	if len(hostedAWSCluster.Spec.ServiceEndpoints) > 0 {
		awsSpec.ServiceEndpoints = make([]hyperv1.AWSServiceEndpoint, len(hostedAWSCluster.Spec.ServiceEndpoints))
		for i, endpoint := range hostedAWSCluster.Spec.ServiceEndpoints {
			awsSpec.ServiceEndpoints[i] = hyperv1.AWSServiceEndpoint{
				Name: endpoint.Name,
				URL:  endpoint.URL,
			}
		}
	}

	// Set roles reference
	awsSpec.RolesRef = hyperv1.AWSRolesRef{
		IngressARN:              hostedAWSCluster.Spec.RolesRef.IngressARN,
		ImageRegistryARN:        hostedAWSCluster.Spec.RolesRef.ImageRegistryARN,
		StorageARN:              hostedAWSCluster.Spec.RolesRef.StorageARN,
		NetworkARN:              hostedAWSCluster.Spec.RolesRef.NetworkARN,
		KubeCloudControllerARN:  hostedAWSCluster.Spec.RolesRef.KubeCloudControllerARN,
		NodePoolManagementARN:   hostedAWSCluster.Spec.RolesRef.NodePoolManagementARN,
		ControlPlaneOperatorARN: hostedAWSCluster.Spec.RolesRef.ControlPlaneOperatorARN,
	}

	// Set resource tags
	if len(hostedAWSCluster.Spec.ResourceTags) > 0 {
		awsSpec.ResourceTags = make([]hyperv1.AWSResourceTag, len(hostedAWSCluster.Spec.ResourceTags))
		for i, tag := range hostedAWSCluster.Spec.ResourceTags {
			awsSpec.ResourceTags[i] = hyperv1.AWSResourceTag{
				Key:   tag.Key,
				Value: tag.Value,
			}
		}
	}

	// Set endpoint access
	awsSpec.EndpointAccess = hyperv1.AWSEndpointAccessType(hostedAWSCluster.Spec.EndpointAccess)

	// Set additional allowed principals
	if len(hostedAWSCluster.Spec.AdditionalAllowedPrincipals) > 0 {
		awsSpec.AdditionalAllowedPrincipals = hostedAWSCluster.Spec.AdditionalAllowedPrincipals
	}

	return awsSpec
}

func controlPlaneNamespace(hostedCluster *hyperv1.HostedCluster) string {
	return fmt.Sprintf("%s-%s", hostedCluster.Namespace, hostedCluster.Name)
}

// reconcileCredentials creates AWS credential secrets in the control plane namespace
// These secrets are used by various control plane components (KCM, CAPI controllers, operators, etc.)
func (r *HostedAWSClusterReconciler) reconcileCredentials(ctx context.Context, hostedAWSCluster *infrav1.HostedAWSCluster, hostedCluster *hyperv1.HostedCluster, controlPlaneNamespace string) error {
	log := log.FromContext(ctx)
	region := hostedAWSCluster.Spec.Region

	// syncSecret creates or updates a credential secret with the given ARN
	syncSecret := func(secret *corev1.Secret, arn string, description string) error {
		credentials, err := buildAWSWebIdentityCredentials(arn, region)
		if err != nil {
			return fmt.Errorf("failed to build %s credentials: %w", description, err)
		}

		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			// Add labels to track the owning HostedAWSCluster (can't use owner reference cross-namespace)
			if secret.Labels == nil {
				secret.Labels = make(map[string]string)
			}
			secret.Labels[labelHostedAWSClusterNamespace] = hostedAWSCluster.Namespace
			secret.Labels[labelHostedAWSClusterName] = hostedAWSCluster.Name

			if secret.Data == nil {
				secret.Data = make(map[string][]byte)
			}
			secret.Data["credentials"] = []byte(credentials)
			secret.Type = corev1.SecretTypeOpaque

			return nil
		})

		if err != nil {
			return fmt.Errorf("failed to reconcile %s secret %s/%s: %w", description, secret.Namespace, secret.Name, err)
		}

		log.V(1).Info("Reconciled credential secret", "secret", secret.Name, "result", result, "description", description)
		return nil
	}

	// Reconcile all required credential secrets
	credentialSecrets := map[string]struct {
		secret      *corev1.Secret
		arn         string
		description string
	}{
		"kube-cloud-controller": {
			secret:      kubeCloudControllerCredsSecret(controlPlaneNamespace),
			arn:         hostedAWSCluster.Spec.RolesRef.KubeCloudControllerARN,
			description: "Kube Cloud Controller",
		},
		"node-pool-management": {
			secret:      nodePoolManagementCredsSecret(controlPlaneNamespace),
			arn:         hostedAWSCluster.Spec.RolesRef.NodePoolManagementARN,
			description: "Node Pool Management (CAPI)",
		},
		"control-plane-operator": {
			secret:      controlPlaneOperatorCredsSecret(controlPlaneNamespace),
			arn:         hostedAWSCluster.Spec.RolesRef.ControlPlaneOperatorARN,
			description: "Control Plane Operator",
		},
		"network": {
			secret:      cloudNetworkConfigControllerCredsSecret(controlPlaneNamespace),
			arn:         hostedAWSCluster.Spec.RolesRef.NetworkARN,
			description: "Cloud Network Config Controller",
		},
		"storage": {
			secret:      awsEBSCSIDriverCredsSecret(controlPlaneNamespace),
			arn:         hostedAWSCluster.Spec.RolesRef.StorageARN,
			description: "AWS EBS CSI Driver",
		},
	}

	var errs []error
	for _, cred := range credentialSecrets {
		if err := syncSecret(cred.secret, cred.arn, cred.description); err != nil {
			errs = append(errs, err)
		}
	}

	// Handle optional KMS credentials for secret encryption
	if hostedCluster.Spec.SecretEncryption != nil &&
		hostedCluster.Spec.SecretEncryption.KMS != nil &&
		hostedCluster.Spec.SecretEncryption.KMS.AWS != nil &&
		hostedCluster.Spec.SecretEncryption.KMS.AWS.ActiveKey.ARN != "" &&
		hostedCluster.Spec.SecretEncryption.KMS.AWS.Auth.AWSKMSRoleARN != "" {

		if err := syncSecret(
			awsKMSCredsSecret(controlPlaneNamespace),
			hostedCluster.Spec.SecretEncryption.KMS.AWS.Auth.AWSKMSRoleARN,
			"AWS KMS",
		); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to reconcile one or more credential secrets: %v", errs)
	}

	log.Info("Successfully reconciled all AWS credential secrets")
	return nil
}

// buildAWSWebIdentityCredentials builds the AWS credentials file content for web identity federation
func buildAWSWebIdentityCredentials(roleArn, region string) (string, error) {
	if roleArn == "" {
		return "", fmt.Errorf("role ARN cannot be empty")
	}
	if region == "" {
		return "", fmt.Errorf("region must be specified for cross-partition compatibility")
	}
	return fmt.Sprintf(awsCredentialsTemplate, roleArn, region), nil
}

// Credential secret helper functions
// These create secret objects with the standard names expected by control plane components

func kubeCloudControllerCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "cloud-controller-creds",
		},
	}
}

func nodePoolManagementCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "node-management-creds",
		},
	}
}

func controlPlaneOperatorCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "control-plane-operator-creds",
		},
	}
}

func cloudNetworkConfigControllerCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "cloud-network-config-controller-creds",
		},
	}
}

func awsEBSCSIDriverCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "ebs-cloud-credentials",
		},
	}
}

func awsKMSCredsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "kms-creds",
		},
	}
}
