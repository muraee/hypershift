package ignitionpayload

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testNS = "hcp"

// fakeMinter mints a deterministic, monotonically-distinct token so tests can detect re-mints.
type fakeMinter struct{ n int }

func (m *fakeMinter) Mint(owner payloadstore.OwnerRef, ttl time.Duration) (string, error) {
	m.n++
	return fmt.Sprintf("token-%s-%s-%d", owner.Namespace, owner.Name, m.n), nil
}

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
			ReleaseImage:         "quay.io/ocp@sha256:abc",
			PullSecret:           hyperv1.SecretReference{Name: "ps"},
			RolloutConfigMaps:    []hyperv1.ConfigMapReference{{Name: "user-a"}, {Name: "core-fips"}},
			ManagementConfigMaps: []hyperv1.ConfigMapReference{{Name: "haproxy"}},
		},
	}
}

func newTestReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *fakeProvider) {
	t.Helper()
	store := payloadstore.NewMemStore()
	fp := &fakeProvider{payload: []byte("PAYLOAD")}
	c := fake.NewClientBuilder().
		WithScheme(api.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).
		Build()
	r := &Reconciler{
		Client:                 c,
		Store:                  store,
		Generator:              &payloadGenerator{store: store, provider: fp},
		Minter:                 &fakeMinter{},
		Namespace:              testNS,
		IgnitionCACert:         []byte("ca"),
		IgnitionServerEndpoint: "ign.example.com",
		TokenTTL:               time.Hour,
	}
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

// TestReconciler_Reconcile exercises the full reconcile state machine; each scenario is a subtest.
// Scenario bodies live in this file plus retirement_test.go and finalfixes_test.go.
func TestReconciler_Reconcile(t *testing.T) {
	t.Run("RolloutThenRefresh", scenarioRolloutThenRefresh)
	t.Run("AToBToCDeleteOnEvict", scenarioAToBToCDeleteOnEvict)
	t.Run("RebaselineDoesNotRoll", scenarioRebaselineDoesNotRoll)
	t.Run("InvalidConfigNoRollout", scenarioInvalidConfigNoRollout)
	t.Run("AddsFinalizer", scenarioAddsFinalizer)
	t.Run("DeletionFreesPayloadsAndRemovesFinalizer", scenarioDeletionFreesPayloadsAndRemovesFinalizer)
	t.Run("RetirementClearsPrevious", scenarioRetirementClearsPrevious)
	t.Run("OrphanSweep", scenarioOrphanSweep)
	t.Run("PullSecretHashContract", scenarioPullSecretHashContract)
	t.Run("TrustBundleRotationMovesIdentity", scenarioTrustBundleRotationMovesIdentity)
	t.Run("MissingConfigMapRequeues", scenarioMissingConfigMapRequeues)
}

func scenarioRolloutThenRefresh(t *testing.T) {
	g := NewWithT(t)
	r, c, fp := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)

	// First reconcile → generation 1, a userData Secret, PayloadGenerated True, one render.
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(1)))
	firstUserData := cr.Status.Current.UserDataSecretName
	g.Expect(firstUserData).To(Equal("np-1-1"))
	g.Expect(apimeta.IsStatusConditionTrue(cr.Status.Conditions, "PayloadGenerated")).To(BeTrue())
	g.Expect(fp.calls).To(Equal(1))

	// Mgmt-only change → refresh: generation unchanged, SAME userData Secret (Policy A), one render.
	updateCM(t, c, "haproxy", mc("00-haproxy-v2"))
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(1)))
	g.Expect(cr.Status.Current.UserDataSecretName).To(Equal(firstUserData))
	g.Expect(fp.calls).To(Equal(2))

	// Reconcile again with NO change → no render, no generation change (idempotent steady state).
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(1)))
	g.Expect(fp.calls).To(Equal(2))

	// Rollout-relevant change → generation 2, NEW userData Secret, previous = old one, reset.
	updateCM(t, c, "user-a", mc("00-user-a-v2"))
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(2)))
	g.Expect(cr.Status.Current.UserDataSecretName).To(Equal("np-1-2"))
	g.Expect(cr.Status.Current.UserDataSecretName).ToNot(Equal(firstUserData))
	g.Expect(cr.Status.Previous.UserDataSecretName).To(Equal(firstUserData))
	g.Expect(apimeta.IsStatusConditionFalse(cr.Status.Conditions, "IgnitionReached")).To(BeTrue())
}

func scenarioAToBToCDeleteOnEvict(t *testing.T) {
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
	updateCM(t, c, "user-a", mc("00-user-a-v2"))
	reconcileCR(t, r) // gen 2 (B)
	updateCM(t, c, "user-a", mc("00-user-a-v3"))
	reconcileCR(t, r) // gen 3 (C): previous was B; A should have been evicted at gen 2's rollout
	cr := getCR(t, c)

	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(3)))
	g.Expect(cr.Status.Previous.RolloutGeneration).To(Equal(int64(2)))
	// At most two store-referenced payloads live (current + previous); A (gen 1) is gone.
	gens, err := r.Store.ListByOwner(ctx, owner)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(gens).To(ConsistOf(int64(2), int64(3)))
	g.Expect(gens).ToNot(ContainElement(int64(1)))
}

func scenarioRebaselineDoesNotRoll(t *testing.T) {
	g := NewWithT(t)
	r, c, _ := newTestReconciler(t,
		testCR(), pullSecret("ps"),
		cfgMap(testNS, "user-a", mc("00-user-a")),
		cfgMap(testNS, "core-fips", mc("00-core-fips")),
		cfgMap(testNS, "haproxy", mc("00-haproxy")),
	)
	reconcileCR(t, r)
	cr := getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(1)))
	g.Expect(cr.Status.RolloutHashVersion).To(Equal(rolloutHashFormulaVersion))

	// Simulate an older stored formula version + a stale rolloutHash.
	cr.Status.RolloutHashVersion = rolloutHashFormulaVersion - 1
	cr.Status.Current.RolloutHash = "STALE"
	g.Expect(c.Status().Update(context.Background(), cr)).To(Succeed())

	// Reconcile → rebaseline: rolloutHash rewritten, version bumped, generation NOT advanced.
	reconcileCR(t, r)
	cr = getCR(t, c)
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(1)), "rebaseline must not advance generation")
	g.Expect(cr.Status.RolloutHashVersion).To(Equal(rolloutHashFormulaVersion))
	g.Expect(cr.Status.Current.RolloutHash).ToNot(Equal("STALE"))
}

func scenarioInvalidConfigNoRollout(t *testing.T) {
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
	g.Expect(cr.Status.Current.UserDataSecretName).To(BeEmpty(), "no userData authored for invalid config")
	g.Expect(cr.Status.Current.RolloutGeneration).To(Equal(int64(0)), "no rollout for invalid config")
	g.Expect(fp.calls).To(Equal(0), "no render for invalid config")
}
