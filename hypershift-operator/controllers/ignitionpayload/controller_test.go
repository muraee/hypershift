package ignitionpayload

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	"github.com/openshift/hypershift/support/releaseinfo"

	imageapi "github.com/openshift/api/image/v1"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeReleaseProvider resolves release-image pullspecs to OCP versions for tests. An image
// absent from versions resolves to "4.23.0"; a non-nil err makes Lookup fail.
type fakeReleaseProvider struct {
	versions map[string]string
	err      error
}

func (f *fakeReleaseProvider) Lookup(_ context.Context, image string, _ []byte) (*releaseinfo.ReleaseImage, error) {
	if f.err != nil {
		return nil, f.err
	}
	v := f.versions[image]
	if v == "" {
		v = "4.23.0"
	}
	return &releaseinfo.ReleaseImage{ImageStream: &imageapi.ImageStream{ObjectMeta: metav1.ObjectMeta{Name: v}}}, nil
}

const testNS = "hcp"

func pullSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
	}
}

func testCR() *hyperv1.IgnitionPayload {
	return &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "np-1"},
		Spec: hyperv1.IgnitionPayloadSpec{
			ReleaseImage:      "quay.io/ocp@sha256:abc",
			PullSecretName:    "ps",
			RolloutConfigMaps: []hyperv1.ConfigMapReference{{Name: "user-a"}, {Name: "core-fips"}},
			MgmtConfigMaps:    []hyperv1.ConfigMapReference{{Name: "haproxy"}},
		},
	}
}

func newTestReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *fakeProvider) {
	store := payloadstore.NewMemStore()
	fp := &fakeProvider{payload: []byte("PAYLOAD")}
	c := fake.NewClientBuilder().
		WithScheme(api.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).
		Build()
	r := &Reconciler{Client: c, Store: store, Generator: &payloadGenerator{store: store, provider: fp}, ReleaseProvider: &fakeReleaseProvider{}, Namespace: testNS}
	return r, c, fp
}

func reconcileCR(t *testing.T, r *Reconciler) {
	t.Helper()
	g := NewWithT(t)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNS, Name: "np-1"}})
	g.Expect(err).ToNot(HaveOccurred())
}

func getCR(t *testing.T, c client.Client) *hyperv1.IgnitionPayload {
	t.Helper()
	g := NewWithT(t)
	cr := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "np-1"}, cr)).To(Succeed())
	return cr
}

func updateCM(t *testing.T, c client.Client, name, cfg string) {
	t.Helper()
	g := NewWithT(t)
	cm := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, cm)).To(Succeed())
	cm.Data[configDataKey] = cfg
	g.Expect(c.Update(context.Background(), cm)).To(Succeed())
}

func TestReconcileRolloutThenRefresh(t *testing.T) {
	g := NewWithT(t)
	r, c, fp := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)

	// First reconcile → generation 1, a token, PayloadGenerated True, one render.
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)))
	firstToken := cr.Status.Current.Token
	g.Expect(firstToken).ToNot(BeEmpty())
	g.Expect(apimeta.IsStatusConditionTrue(cr.Status.Conditions, "PayloadGenerated")).To(BeTrue())
	g.Expect(fp.calls).To(Equal(1))

	// Mgmt-only change → refresh: generation unchanged, SAME token (Policy A), one more render.
	updateCM(t, c, "haproxy", mc("00-haproxy-v2"))
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)))
	g.Expect(cr.Status.Current.Token).To(Equal(firstToken))
	g.Expect(fp.calls).To(Equal(2))

	// Reconcile again with NO change → no render, no generation change (idempotent steady state).
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)))
	g.Expect(fp.calls).To(Equal(2))

	// Rollout-relevant change → generation 2, NEW token, previous = old token, IgnitionReached reset.
	updateCM(t, c, "user-a", mc("00-user-a-v2"))
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(2)))
	g.Expect(cr.Status.Current.Token).ToNot(Equal(firstToken))
	g.Expect(cr.Status.Previous.Token).To(Equal(firstToken))
	g.Expect(apimeta.IsStatusConditionFalse(cr.Status.Conditions, "IgnitionReached")).To(BeTrue())
}

func TestReconcileAToBToCDeleteOnEvict(t *testing.T) {
	g := NewWithT(t)
	r, c, _ := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	ctx := context.Background()
	owner := payloadstore.OwnerRef{Namespace: testNS, Name: "np-1"}

	reconcileCR(t, r) // gen 1 (A)
	tokenA := getCR(t, c).Status.Current.Token
	updateCM(t, c, "user-a", mc("00-user-a-v2"))
	reconcileCR(t, r) // gen 2 (B)
	tokenB := getCR(t, c).Status.Current.Token
	updateCM(t, c, "user-a", mc("00-user-a-v3"))
	reconcileCR(t, r) // gen 3 (C): previous was B; A should have been evicted at gen 2's rollout
	cr := getCR(t, c)
	tokenC := cr.Status.Current.Token

	g.Expect(cr.Status.Current.Generation).To(Equal(int64(3)))
	g.Expect(cr.Status.Previous.Token).To(Equal(tokenB))
	// At most two store-referenced tokens live (current + previous); A is gone.
	toks, err := r.Store.ListByOwner(ctx, owner)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(toks).To(ConsistOf(tokenB, tokenC))
	g.Expect(toks).ToNot(ContainElement(tokenA))
}

func TestReconcileRebaselineDoesNotRoll(t *testing.T) {
	g := NewWithT(t)
	r, c, _ := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)))
	g.Expect(cr.Status.RolloutHashVersion).To(Equal(rolloutHashFormulaVersion))

	// Simulate an older stored formula version + a stale rolloutHash.
	cr.Status.RolloutHashVersion = rolloutHashFormulaVersion - 1
	cr.Status.Current.RolloutHash = "STALE"
	g.Expect(c.Status().Update(context.Background(), cr)).To(Succeed())

	// Reconcile → rebaseline: rolloutHash rewritten, version bumped, generation NOT advanced.
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)), "rebaseline must not advance generation")
	g.Expect(cr.Status.RolloutHashVersion).To(Equal(rolloutHashFormulaVersion))
	g.Expect(cr.Status.Current.RolloutHash).ToNot(Equal("STALE"))
}

func TestReconcileInvalidConfigNoRollout(t *testing.T) {
	g := NewWithT(t)
	r, c, fp := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", "apiVersion: v1\nkind: NotAThing\n"),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(apimeta.IsStatusConditionFalse(cr.Status.Conditions, "PayloadGenerated")).To(BeTrue())
	g.Expect(cr.Status.Current.Token).To(BeEmpty(), "no token minted for invalid config")
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(0)), "no rollout for invalid config")
	g.Expect(fp.calls).To(Equal(0), "no render for invalid config")
}

// TestReconcileReleaseVersionHashing pins RF#2: the rollout hash keys on the resolved OCP
// Version(), not the release pullspec — matching the deployed ConfigGenerator.Hash(). A pullspec
// change that resolves to the same version must NOT roll; a different version must roll.
func TestReconcileReleaseVersionHashing(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, c, _ := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	r.ReleaseProvider = &fakeReleaseProvider{versions: map[string]string{
		"quay.io/ocp@sha256:abc": "4.23.0", // testCR()'s image
		"quay.io/ocp@sha256:def": "4.23.0", // different pullspec, SAME version
		"quay.io/ocp@sha256:xyz": "4.24.0", // different version
	}}

	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)))
	roll1 := cr.Status.Current.RolloutHash

	// Same version, different pullspec → no roll.
	cr.Spec.ReleaseImage = "quay.io/ocp@sha256:def"
	g.Expect(c.Update(ctx, cr)).To(Succeed())
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(1)), "same resolved version must not roll")
	g.Expect(cr.Status.Current.RolloutHash).To(Equal(roll1))

	// Different version → roll.
	cr.Spec.ReleaseImage = "quay.io/ocp@sha256:xyz"
	g.Expect(c.Update(ctx, cr)).To(Succeed())
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.Generation).To(Equal(int64(2)), "new resolved version must roll")
}

// TestReconcileReleaseLookupErrorRequeues pins RF#5: a release Lookup failure is transient —
// Reconcile returns an error (requeue), mints no token, and does not set PayloadGenerated=False.
func TestReconcileReleaseLookupErrorRequeues(t *testing.T) {
	g := NewWithT(t)
	r, c, fp := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	r.ReleaseProvider = &fakeReleaseProvider{err: errors.New("registry unavailable")}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNS, Name: "np-1"}})
	g.Expect(err).To(HaveOccurred())

	cr := getCR(t, c)
	g.Expect(cr.Status.Current.Token).To(BeEmpty(), "no token on transient Lookup failure")
	g.Expect(fp.calls).To(Equal(0), "no render on transient Lookup failure")
	g.Expect(apimeta.IsStatusConditionFalse(cr.Status.Conditions, "PayloadGenerated")).To(BeFalse(), "transient failure is not a validation failure")
}
