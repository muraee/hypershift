package ignitionpayloadserver

import (
	"context"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hypershiftv1ac "github.com/openshift/hypershift/client/applyconfiguration/hypershift/v1beta1"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	fieldManager             = "ignition-payload-server"
	conditionIgnitionReached = "IgnitionReached"
)

// SetIgnitionReached sets the IgnitionReached condition to True on the owning IgnitionPayload CR,
// but ONLY when the served token still equals status.current.token (so a drained previous token,
// or a serve after the generation advanced, does not flip it) and it is not already True. It uses
// a field-scoped Server-Side Apply under its own field manager, so it never clobbers the
// generator-owned status.current/previous/PayloadGenerated.
func SetIgnitionReached(ctx context.Context, c client.Client, owner payloadstore.OwnerRef, token string) error {
	cr := &hyperv1.IgnitionPayload{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, cr); err != nil {
		return client.IgnoreNotFound(err)
	}

	// Gate: only the current generation's token flips the condition.
	if cr.Status.Current.Token != token {
		return nil
	}
	if apimeta.IsStatusConditionTrue(cr.Status.Conditions, conditionIgnitionReached) {
		return nil
	}

	applyCfg := hypershiftv1ac.IgnitionPayload(owner.Name, owner.Namespace).
		WithStatus(hypershiftv1ac.IgnitionPayloadStatus().
			WithConditions(metav1ac.Condition().
				WithType(conditionIgnitionReached).
				WithStatus(metav1.ConditionTrue).
				WithReason("Served").
				WithMessage("ignition payload served to a node").
				WithLastTransitionTime(metav1.Now()).
				WithObservedGeneration(cr.Status.Current.Generation)))

	return c.Status().Apply(ctx, applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership)
}
