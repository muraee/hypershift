package ignitionpayloadserver

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func reachedTestCR(name string) *hyperv1.IgnitionPayload {
	return &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hcp", Name: name},
		Status: hyperv1.IgnitionPayloadStatus{
			Current:  hyperv1.PayloadReference{ConfigHash: "h", RolloutHash: "r", Token: "cur", Generation: 2},
			Previous: hyperv1.PayloadReference{ConfigHash: "h0", RolloutHash: "r0", Token: "prev", Generation: 1},
		},
	}
}

func TestSetIgnitionReachedCurrentToken(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := reachedTestCR("np-1")
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(cr).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).Build()

	// Serving the CURRENT token sets IgnitionReached True and leaves current/previous untouched.
	g.Expect(SetIgnitionReached(ctx, c, payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}, "cur")).To(Succeed())

	got := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), got)).To(Succeed())
	g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, "IgnitionReached")).To(BeTrue())
	g.Expect(got.Status.Current.Token).To(Equal("cur"))
	g.Expect(got.Status.Previous.Token).To(Equal("prev"))
}

func TestSetIgnitionReachedPreviousTokenDoesNotFlip(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := reachedTestCR("np-2")
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(cr).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).Build()

	// Serving the PREVIOUS (drained) token must NOT set IgnitionReached.
	g.Expect(SetIgnitionReached(ctx, c, payloadstore.OwnerRef{Namespace: "hcp", Name: "np-2"}, "prev")).To(Succeed())

	got := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), got)).To(Succeed())
	g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, "IgnitionReached")).To(BeFalse())
}
