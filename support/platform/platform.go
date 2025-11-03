package platform

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetAWSPlatformSpec retrieves the AWS platform specification from either:
// 1. The provider-managed HostedAWSCluster CR (if infrastructureRef is set)
// 2. The inline HostedCluster.Spec.Platform.AWS
//
// This allows controllers to work transparently with both legacy and provider-managed infrastructure.
func GetAWSPlatformSpec(ctx context.Context, c client.Client, hcluster *hyperv1.HostedCluster) (*hyperv1.AWSPlatformSpec, error) {
	if hcluster.Spec.InfrastructureRef != nil {
		// Provider-managed infrastructure mode
		return getAWSPlatformSpecFromProvider(ctx, c, hcluster)
	}

	// Legacy inline mode
	if hcluster.Spec.Platform.AWS == nil {
		return nil, fmt.Errorf("HostedCluster has no AWS platform spec and no infrastructureRef")
	}
	return hcluster.Spec.Platform.AWS, nil
}

// getAWSPlatformSpecFromProvider retrieves the AWS platform spec from the HostedAWSCluster CR
func getAWSPlatformSpecFromProvider(ctx context.Context, c client.Client, hcluster *hyperv1.HostedCluster) (*hyperv1.AWSPlatformSpec, error) {
	// Get the provider CR (HostedAWSCluster)
	providerCR := &unstructured.Unstructured{}
	providerCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   hcluster.Spec.InfrastructureRef.GroupVersionKind().Group,
		Version: hcluster.Spec.InfrastructureRef.GroupVersionKind().Version,
		Kind:    hcluster.Spec.InfrastructureRef.Kind,
	})

	if err := c.Get(ctx, client.ObjectKey{
		Namespace: hcluster.Spec.InfrastructureRef.Namespace,
		Name:      hcluster.Spec.InfrastructureRef.Name,
	}, providerCR); err != nil {
		return nil, fmt.Errorf("failed to get provider CR: %w", err)
	}

	// Extract the spec from the provider CR
	specData, found, err := unstructured.NestedMap(providerCR.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("failed to get spec from provider CR: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("provider CR has no spec")
	}

	// Convert the spec map to AWSPlatformSpec using runtime conversion
	awsSpec := &hyperv1.AWSPlatformSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(specData, awsSpec); err != nil {
		return nil, fmt.Errorf("failed to convert provider spec to AWSPlatformSpec: %w", err)
	}

	return awsSpec, nil
}

// IsUsingProviderManagedInfrastructure returns true if the HostedCluster is using
// provider-managed infrastructure (i.e., has an infrastructureRef set)
func IsUsingProviderManagedInfrastructure(hcluster *hyperv1.HostedCluster) bool {
	return hcluster.Spec.InfrastructureRef != nil
}

// GetInfrastructureRef returns the infrastructureRef if set, otherwise nil
func GetInfrastructureRef(hcluster *hyperv1.HostedCluster) *corev1.ObjectReference {
	return hcluster.Spec.InfrastructureRef
}
