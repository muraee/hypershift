package nodepool

import (
	"context"
	"fmt"
	"strconv"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// nodePoolAnnotationIgnitionDeployedGeneration records the IgnitionPayload rolloutGeneration a
// NodePool's current nodes are on. It is the one-time baseline that makes gate enablement no-roll: the
// adoption decision compares status.current.rolloutGeneration against it rather than against any hash,
// so a legacy-vs-new rollout-hash-formula difference can never trigger a spurious fleet roll.
// (Enhancement §"Adoption (no roll) and the deployed-generation baseline".)
const nodePoolAnnotationIgnitionDeployedGeneration = "hypershift.openshift.io/ignition-payload-deployed-generation"

// adoptionDecision is the outcome of the no-roll adoption algorithm for one NodePool.
type adoptionDecision struct {
	// userDataSecretName is the Secret name CAPI points the pool's Machines at: the adopted live name
	// (no roll) or the canonical <ignitionpayload-name>-<rolloutGeneration> (genuine rollout).
	userDataSecretName string
	// inPlaceConfigVersion is the config version to stamp on an InPlace MachineSet's targetConfigVersion
	// annotation. Set only for a genuine rollout (or a brand-new pool); empty during adoption/steady so
	// HCCO does not reboot already-joined nodes.
	inPlaceConfigVersion string
	// copyContent is true when the generator-authored userdata content must be copied under
	// userDataSecretName (the adopted live name). False when userDataSecretName is already the
	// generator's canonical Secret.
	copyContent bool
	// sweepInFlight is true on the first adoption of an already-provisioned pool, when not-yet-joined
	// Machines created with stale userdata must be recreated off the refreshed content.
	sweepInFlight bool
}

// resolveAdoption implements the no-roll adoption algorithm and records the deployed-generation
// baseline on nodePool (persisted by the caller's NodePool patch). existing is the pool's live CAPI
// Bootstrap.DataSecretName ("" for a never-provisioned pool). current is the generator's published
// status.current; the caller must only invoke this once current.UserDataSecret.Name is set.
func resolveAdoption(nodePool *hyperv1.NodePool, current hyperv1.PayloadReference, existing string) adoptionDecision {
	canonical := current.UserDataSecret.Name
	baselineStr, hasBaseline := nodePool.GetAnnotations()[nodePoolAnnotationIgnitionDeployedGeneration]

	setBaseline := func() {
		if nodePool.Annotations == nil {
			nodePool.Annotations = map[string]string{}
		}
		nodePool.Annotations[nodePoolAnnotationIgnitionDeployedGeneration] = strconv.FormatInt(current.RolloutGeneration, 10)
	}

	switch {
	case existing == "":
		// Brand-new pool: nothing to adopt. Born on canonical naming at the current generation; a
		// fresh InPlace MachineSet initializes its config version so the first evaluation is a no-op.
		setBaseline()
		return adoptionDecision{userDataSecretName: canonical, inPlaceConfigVersion: current.ConfigHash}
	case !hasBaseline:
		// First encounter of an already-provisioned pool → adopt, no roll: keep the live name, copy the
		// new content under it, and sweep any in-flight Machine still carrying stale userdata.
		setBaseline()
		return adoptionDecision{userDataSecretName: existing, copyContent: true, sweepInFlight: true}
	}

	baseline, _ := strconv.ParseInt(baselineStr, 10, 64)
	if current.RolloutGeneration > baseline {
		// Genuine rollout after adoption → re-point to the canonical name; CAPI rolls.
		setBaseline()
		return adoptionDecision{userDataSecretName: canonical, inPlaceConfigVersion: current.ConfigHash}
	}

	// Steady (generation == baseline, or a stale generation): keep the live name and keep its content
	// synced to the generator's (token re-issue, management-side refresh). No re-point, no roll.
	return adoptionDecision{userDataSecretName: existing, copyContent: true}
}

// copyUserDataSecret mirrors the generator-authored userdata Secret (src) into a Secret named dst in
// the same namespace, owned by the IgnitionPayload CR so it is garbage-collected with the payload. It
// is the "refresh in place" half of adoption: dst keeps the live name CAPI already references (so no
// roll) while carrying the new-model content (pointer ignition to the reused endpoint + signed JWT),
// which a scale-up Machine reads. A no-op when src == dst (dst is already the generator's Secret).
func copyUserDataSecret(ctx context.Context, c client.Client, namespace, src, dst string, owner *hyperv1.IgnitionPayload) error {
	if src == dst {
		return nil
	}
	source := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: src}, source); err != nil {
		return fmt.Errorf("failed to read generator userdata Secret %s/%s: %w", namespace, src, err)
	}

	target := &corev1.Secret{}
	target.Namespace = namespace
	target.Name = dst
	if _, err := controllerutil.CreateOrUpdate(ctx, c, target, func() error {
		target.Type = source.Type
		target.Data = source.Data
		return controllerutil.SetControllerReference(owner, target, c.Scheme())
	}); err != nil {
		return fmt.Errorf("failed to copy userdata content into adopted Secret %s/%s: %w", namespace, dst, err)
	}
	return nil
}
