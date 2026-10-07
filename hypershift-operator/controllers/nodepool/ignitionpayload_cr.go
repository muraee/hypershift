package nodepool

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// consumerFinalizer guards the NodePool controller's IgnitionPayload CR so the
// controller can tear down its projected ConfigMaps and free the store token before
// the CR is removed.
const consumerFinalizer = "hypershift.openshift.io/ignition-payload-consumer"

// ignitionPayloadSpec assembles the consumer inputs into an IgnitionPayloadSpec. It
// does not set retiredGeneration, which is owned by advanceRetiredGeneration and
// preserved across spec reconciles.
func ignitionPayloadSpec(nodePool *hyperv1.NodePool, hc *hyperv1.HostedCluster,
	rolloutRefs, mgmtRefs []hyperv1.ConfigMapReference, rolloutGlobalConfigName, osStream string) hyperv1.IgnitionPayloadSpec {
	spec := hyperv1.IgnitionPayloadSpec{
		ReleaseImage:        nodePool.Spec.Release.Image,
		PullSecretName:      hc.Spec.PullSecret.Name,
		OSStream:            osStream,
		RolloutGlobalConfig: hyperv1.ConfigMapReference{Name: rolloutGlobalConfigName},
		RolloutConfigMaps:   rolloutRefs,
		MgmtConfigMaps:      mgmtRefs,
	}
	if hc.Spec.AdditionalTrustBundle != nil {
		spec.AdditionalTrustBundle = hyperv1.ConfigMapReference{Name: hc.Spec.AdditionalTrustBundle.Name}
	}
	return spec
}

// reconcileIgnitionPayloadCR creates or updates the per-NodePool IgnitionPayload CR in
// hcpNamespace. The CR is named after the NodePool, carries the back-reference
// annotation (ns/name) used by the reverse watch, and holds the consumer finalizer.
// retiredGeneration is preserved across updates.
func reconcileIgnitionPayloadCR(ctx context.Context, c client.Client, hcpNamespace string,
	nodePool *hyperv1.NodePool, spec hyperv1.IgnitionPayloadSpec) (*hyperv1.IgnitionPayload, error) {
	cr := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: nodePool.GetName(), Namespace: hcpNamespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, cr, func() error {
		if cr.Annotations == nil {
			cr.Annotations = map[string]string{}
		}
		cr.Annotations[nodePoolAnnotation] = client.ObjectKeyFromObject(nodePool).String()
		if cr.Labels == nil {
			cr.Labels = map[string]string{}
		}
		cr.Labels[hyperv1.NodePoolLabel] = nodePool.GetName()

		if !sets.New(cr.Finalizers...).Has(consumerFinalizer) {
			cr.Finalizers = append(cr.Finalizers, consumerFinalizer)
		}

		// retiredGeneration is owned by advanceRetiredGeneration; carry the existing
		// value so a spec reconcile never resets it.
		retired := cr.Spec.RetiredGeneration
		cr.Spec = spec
		cr.Spec.RetiredGeneration = retired
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to reconcile IgnitionPayload %s/%s: %w", hcpNamespace, nodePool.GetName(), err)
	}
	return cr, nil
}
