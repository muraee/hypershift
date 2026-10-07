package ignitionpayload

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func coreObjs() []client.Object {
	return []client.Object{
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	}
}

func TestReconcileAddsFinalizer(t *testing.T) {
	g := NewWithT(t)
	r, c, _ := newTestReconciler(t, coreObjs()...)
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(controllerutil.ContainsFinalizer(cr, storeCleanupFinalizer)).To(BeTrue())
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)), "first reconcile still generates")
}

func TestReconcileDeletionFreesTokensAndRemovesFinalizer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, c, _ := newTestReconciler(t, coreObjs()...)
	owner := payloadstore.OwnerRef{Namespace: testNS, Name: "np-1"}

	reconcileCR(t, r) // gen 1 + finalizer
	toks, _ := r.Store.ListByOwner(ctx, owner)
	g.Expect(toks).To(HaveLen(1))

	// Delete the CR: with the finalizer it is marked for deletion, not removed.
	cr := getCR(t, c)
	g.Expect(c.Delete(ctx, cr)).To(Succeed())

	// Reconcile observes deletionTimestamp → frees store tokens + removes finalizer → CR gone.
	reconcileCR(t, r)
	toks, _ = r.Store.ListByOwner(ctx, owner)
	g.Expect(toks).To(BeEmpty(), "all owner tokens freed on teardown")
	err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "np-1"}, &hyperv1.IgnitionPayload{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "CR gone after finalizer removed")
}

func TestReconcileRetirementClearsPrevious(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, c, _ := newTestReconciler(t, coreObjs()...)
	owner := payloadstore.OwnerRef{Namespace: testNS, Name: "np-1"}

	reconcileCR(t, r) // gen 1
	updateCM(t, c, "user-a", mc("00-user-a-v2"))
	reconcileCR(t, r) // gen 2: previous = gen-1 token
	cr := getCR(t, c)
	g.Expect(cr.Status.Previous.Generation).To(Equal(int64(1)))
	prevToken := cr.Status.Previous.Token
	g.Expect(prevToken).ToNot(BeEmpty())

	// Signal that generation 1 has drained.
	cr.Spec.RetiredGeneration = 1
	g.Expect(c.Update(ctx, cr)).To(Succeed())

	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Previous.Token).To(BeEmpty(), "previous cleared once retired")
	// The retired token is freed from the store (only current remains).
	toks, _ := r.Store.ListByOwner(ctx, owner)
	g.Expect(toks).To(ConsistOf(cr.Status.Current.Token))
	g.Expect(toks).ToNot(ContainElement(prevToken))
}

func TestReconcileOrphanSweep(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, _, _ := newTestReconciler(t, coreObjs()...)
	owner := payloadstore.OwnerRef{Namespace: testNS, Name: "np-1"}

	// Inject an orphan entry (e.g. an interrupted generation that wrote before the status update).
	g.Expect(r.Store.Put(ctx, owner, "orphan-tok", "orphan-id", []byte("x"))).To(Succeed())

	reconcileCR(t, r) // gen 1
	toks, _ := r.Store.ListByOwner(ctx, owner)
	g.Expect(toks).ToNot(ContainElement("orphan-tok"), "orphan swept")
	g.Expect(toks).To(HaveLen(1), "only the current token remains")
}
