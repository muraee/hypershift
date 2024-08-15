package controlplanecomponent

import (
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/config"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

type DeploymentReconciler interface {
	NamedComponent
	ReconcileDeployment(cpContext ControlPlaneContext, deployment *appsv1.Deployment) error
	Volumes(cpContext ControlPlaneContext) Volumes
}

func (c *controlPlaneWorkload) reconcileDeployment(cpContext ControlPlaneContext) error {
	hcp := cpContext.HCP
	ownerRef := config.OwnerRefFrom(hcp)

	deployment := deploymentManifest(c.Name(), hcp.Namespace)
	if _, err := cpContext.CreateOrUpdate(cpContext, cpContext.Client, deployment, func() error {
		ownerRef.ApplyTo(deployment)

		// preserve existing resource requirements, this needs to be done before calling c.reconcileDeployment() which might override the resources requirements.
		existingResources := make(map[string]corev1.ResourceRequirements)
		for _, container := range deployment.Spec.Template.Spec.Containers {
			existingResources[container.Name] = container.Resources
		}
		// preserve old label selector if it exist, this field is immutable and shouldn't be changed for the lifecycle of the component.
		existingLabelSelector := deployment.Spec.Selector.DeepCopy()

		if err := c.deploymentReconciler.ReconcileDeployment(cpContext, deployment); err != nil {
			return err
		}

		return c.applyOptionsToDeployment(cpContext, deployment, existingResources, existingLabelSelector)
	}); err != nil {
		return fmt.Errorf("failed to reconcile component's deployment: %v", err)
	}

	return nil
}

func (c *controlPlaneWorkload) isDeploymentReady(cpContext ControlPlaneContext) (status metav1.ConditionStatus, reason string, message string) {
	status = metav1.ConditionFalse

	deployment := deploymentManifest(c.Name(), cpContext.HCP.Namespace)
	if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			reason = "Error"
			message = err.Error()
		} else {
			reason = hyperv1.NotFoundReason
			message = fmt.Sprintf("%s Deployment not found", deployment.Name)
		}
		return
	}

	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable {
			if cond.Status == corev1.ConditionTrue {
				status = metav1.ConditionTrue
				reason = hyperv1.AsExpectedReason
				message = fmt.Sprintf("%s Deployment is available", deployment.Name)
			} else {
				reason = hyperv1.WaitingForAvailableReason
				message = fmt.Sprintf("%s Deployment is not available: %s", deployment.Name, cond.Message)
			}
		}
	}

	return
}

func (c *controlPlaneWorkload) applyOptionsToDeployment(cpContext ControlPlaneContext, deployment *appsv1.Deployment, existingResources map[string]corev1.ResourceRequirements, existingLabelSelector *metav1.LabelSelector) error {
	// apply volumes first, as deploymentConfig checks for local volumes to set PodSafeToEvictLocalVolumesKey annotation
	c.deploymentReconciler.Volumes(cpContext).ApplyTo(&deployment.Spec.Template.Spec)

	deploymentConfig := c.defaultDeploymentConfig(cpContext, deployment.Spec.Replicas)
	deploymentConfig.Resources = existingResources
	deploymentConfig.ApplyTo(deployment)

	deployment.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(c.needsManagementKASAccess)
	if existingLabelSelector != nil {
		deployment.Spec.Selector = existingLabelSelector
	}

	if c.konnectivityContainerOpts != nil {
		c.konnectivityContainerOpts.injectKonnectivityContainer(cpContext, &deployment.Spec.Template.Spec)
	}

	return c.applyWatchedResourcesAnnotation(cpContext, &deployment.Spec.Template)
}
