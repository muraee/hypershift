package hostedcluster

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileProviderInfrastructure fetches the provider CR, ensures ownership,
// and extracts CAPI infrastructure CR reference from the provider CR status.
func (r *HostedClusterReconciler) reconcileProviderInfrastructure(ctx context.Context, hcluster *hyperv1.HostedCluster) (*corev1.ObjectReference, error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the provider CR once
	providerCR, err := r.fetchProviderCR(ctx, hcluster.Spec.InfrastructureRef)
	if err != nil {
		return nil, err
	}

	// Ensure ownership
	ownershipUpdated, err := r.ensureProviderOwnership(ctx, hcluster, providerCR)
	if err != nil {
		return nil, err
	}

	if ownershipUpdated {
		log.Info("Set HostedCluster as owner of infrastructure resource",
			"infrastructure", providerCR.GetName(),
			"owner", hcluster.Name)
	}

	// Extract CAPI infrastructure CR reference from status
	capiInfraCRRef, err := r.extractInfrastructureCRRef(providerCR)
	if err != nil {
		return nil, err
	}

	return capiInfraCRRef, nil
}

// fetchProviderCR fetches the provider CR referenced in the ObjectReference
func (r *HostedClusterReconciler) fetchProviderCR(ctx context.Context, ref *corev1.ObjectReference) (*unstructured.Unstructured, error) {
	providerCR := &unstructured.Unstructured{}

	// Parse the APIVersion to get Group and Version
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to parse APIVersion %s: %w", ref.APIVersion, err)
	}

	providerCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    ref.Kind,
	})

	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}, providerCR); err != nil {
		return nil, fmt.Errorf("failed to get provider CR %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return providerCR, nil
}

// ensureProviderOwnership ensures the HostedCluster owns the provider CR
// Returns true if ownership was updated, false if it was already correct
func (r *HostedClusterReconciler) ensureProviderOwnership(ctx context.Context, hcluster *hyperv1.HostedCluster, providerCR *unstructured.Unstructured) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check existing owner references
	ownerRefs := providerCR.GetOwnerReferences()
	var hasCorrectOwner bool
	var hasConflictingOwner bool

	for _, ownerRef := range ownerRefs {
		if ownerRef.Kind == "HostedCluster" && ownerRef.APIVersion == hyperv1.GroupVersion.String() {
			if ownerRef.UID == hcluster.UID {
				// Already owned by this HostedCluster
				hasCorrectOwner = true
				break
			} else {
				// Owned by a different HostedCluster
				hasConflictingOwner = true
				log.Error(fmt.Errorf("infrastructure resource already owned by different HostedCluster"),
					"infrastructure", providerCR.GetName(),
					"currentOwner", ownerRef.Name,
					"attemptedOwner", hcluster.Name)
				break
			}
		}
	}

	if hasConflictingOwner {
		return false, fmt.Errorf("infrastructure resource %s/%s is already owned by a different HostedCluster",
			providerCR.GetNamespace(), providerCR.GetName())
	}

	if hasCorrectOwner {
		// Already correctly owned, nothing to do
		return false, nil
	}

	// Set HostedCluster as controller owner
	// Use controller=true and blockOwnerDeletion=true to enforce 1-to-1 relationship
	newOwnerRef := metav1.OwnerReference{
		APIVersion:         hyperv1.GroupVersion.String(),
		Kind:               "HostedCluster",
		Name:               hcluster.Name,
		UID:                hcluster.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}

	providerCR.SetOwnerReferences(append(ownerRefs, newOwnerRef))

	if err := r.Client.Update(ctx, providerCR); err != nil {
		return false, fmt.Errorf("failed to set owner reference on provider CR: %w", err)
	}

	return true, nil
}

// extractInfrastructureCRRef extracts the CAPI infrastructure CR reference from the provider CR status
func (r *HostedClusterReconciler) extractInfrastructureCRRef(providerCR *unstructured.Unstructured) (*corev1.ObjectReference, error) {
	// Extract the infrastructureCRRef from the provider CR status
	infraCRRef, found, err := unstructured.NestedFieldCopy(providerCR.Object, "status", "infrastructureCRRef")
	if err != nil {
		return nil, fmt.Errorf("failed to get infrastructureCRRef from provider CR status: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("infrastructureCRRef not found in provider CR status, provider controller may not have reconciled yet")
	}

	// infraCRRef should be an ObjectReference
	infraCRRefMap, ok := infraCRRef.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("infrastructureCRRef is not a valid object reference")
	}

	// Extract fields from the ObjectReference and construct the reference
	apiVersion, _, _ := unstructured.NestedString(infraCRRefMap, "apiVersion")
	kind, _, _ := unstructured.NestedString(infraCRRefMap, "kind")
	namespace, _, _ := unstructured.NestedString(infraCRRefMap, "namespace")
	name, _, _ := unstructured.NestedString(infraCRRefMap, "name")

	return &corev1.ObjectReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
	}, nil
}
