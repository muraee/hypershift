package autoscaler

import (
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/kas"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ComponentName = "cluster-autoscaler"

	ImageStreamAutoscalerImage = "cluster-autoscaler"

	kubeconfigVolumeName = "kubeconfig"
)

var _ component.DeploymentReconciler = &AutoscalerReconciler{}

type AutoscalerReconciler struct {
}

func NewComponent() component.ControlPlaneComponent {
	return component.NewDeploymentComponent(&AutoscalerReconciler{}).
		WithRBAC(autoscalerRoleRules()).
		WithPredicate(Predicate).
		NeedsManagementKASAccess().
		Build()
}

// Name implements controlplanecomponent.DeploymentReconciler.
func (a *AutoscalerReconciler) Name() string {
	return ComponentName
}

// Volumes implements controlplanecomponent.DeploymentReconciler.
func (a *AutoscalerReconciler) Volumes(cpContext component.ControlPlaneContext) component.Volumes {
	return nil
}

func Predicate(cpContext component.ControlPlaneContext) (bool, error) {
	hcp := cpContext.HCP

	// Disable cluster-autoscaler component if DisableMachineManagement label is set.
	if _, exists := hcp.Annotations[hyperv1.DisableMachineManagement]; exists {
		return false, nil
	}

	// The deployment depends on the kubeconfig being reported.
	if hcp.Status.KubeConfig == nil {
		return false, nil
	}
	// Resolve the kubeconfig secret for CAPI which the autoscaler is deployed alongside of.
	capiKubeConfigSecret := manifests.KASServiceCAPIKubeconfigSecret(hcp.Namespace, hcp.Spec.InfraID)
	if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(capiKubeConfigSecret), capiKubeConfigSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get hosted controlplane kubeconfig secret %q: %w", capiKubeConfigSecret.Name, err)
	}

	return true, nil
}

// reconcileDeployment implements controlplanecomponent.DeploymentReconciler.
func (a *AutoscalerReconciler) ReconcileDeployment(cpContext component.ControlPlaneContext, deployment *appsv1.Deployment) error {
	hcp := cpContext.HCP

	util.UpdateContainer(ComponentName, deployment.Spec.Template.Spec.Containers, func(c *corev1.Container) {
		c.Image = cpContext.ReleaseImageProvider.GetImage(ImageStreamAutoscalerImage)

		// TODO if the options for the cluster autoscaler continues to grow, we should take inspiration
		// from the cluster-autoscaler-operator and create some utility functions for these assignments.
		options := cpContext.HCP.Spec.Autoscaling
		if options.MaxNodesTotal != nil {
			c.Args = append(c.Args, fmt.Sprintf("--max-nodes-total=%d", *options.MaxNodesTotal))
		}

		if options.MaxPodGracePeriod != nil {
			c.Args = append(c.Args, fmt.Sprintf("--max-graceful-termination-sec=%d", *options.MaxPodGracePeriod))
		}

		if options.MaxNodeProvisionTime != "" {
			c.Args = append(c.Args, fmt.Sprintf("--max-node-provision-time=%s", options.MaxNodeProvisionTime))
		}

		if options.PodPriorityThreshold != nil {
			c.Args = append(c.Args, fmt.Sprintf("--expendable-pods-priority-cutoff=%d", *options.PodPriorityThreshold))
		}
	})

	util.UpdateVolume(kubeconfigVolumeName, deployment.Spec.Template.Spec.Volumes, func(v *corev1.Volume) {
		v.Secret.SecretName = manifests.KASServiceCAPIKubeconfigSecret(hcp.Namespace, hcp.Spec.InfraID).Name
	})

	availabilityProberImage := cpContext.ReleaseImageProvider.GetImage(util.AvailabilityProberImageName)
	util.AvailabilityProber(kas.InClusterKASReadyURL(hcp.Spec.Platform.Type), availabilityProberImage, &deployment.Spec.Template.Spec)

	deployment.Spec.Replicas = ptr.To[int32](1)
	if _, exists := hcp.Annotations[hyperv1.DisableClusterAutoscalerAnnotation]; exists {
		deployment.Spec.Replicas = ptr.To[int32](0)
	}

	return nil
}

func autoscalerRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"cluster.x-k8s.io"},
			Resources: []string{
				"machinedeployments",
				"machinedeployments/scale",
				"machines",
				"machinesets",
				"machinesets/scale",
				"machinepools",
				"machinepools/scale",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{"infrastructure.cluster.x-k8s.io"},
			Resources: []string{"*"},
			Verbs:     []string{"get", "list"},
		},
		{
			APIGroups: []string{"capi-provider.agent-install.openshift.io"},
			Resources: []string{"agentmachinetemplates"},
			Verbs:     []string{"get", "list"},
		},
	}
}
