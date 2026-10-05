package nodepool

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnqueueNodePoolForIgnitionPayload(t *testing.T) {
	g := NewWithT(t)
	r := &NodePoolReconciler{}

	withAnno := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: "hcp", Annotations: map[string]string{nodePoolAnnotation: "clusters/np-1"}},
	}
	reqs := r.enqueueNodePoolForIgnitionPayload(context.Background(), withAnno)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Namespace).To(Equal("clusters"))
	g.Expect(reqs[0].Name).To(Equal("np-1"))

	// A CR without the back-reference annotation is ignored (foreign/unlabeled).
	noAnno := &hyperv1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "hcp"}}
	g.Expect(r.enqueueNodePoolForIgnitionPayload(context.Background(), noAnno)).To(BeEmpty())

	// A malformed annotation is ignored rather than producing a bogus request.
	bad := &hyperv1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "hcp", Annotations: map[string]string{nodePoolAnnotation: "no-slash"}}}
	g.Expect(r.enqueueNodePoolForIgnitionPayload(context.Background(), bad)).To(BeEmpty())
}

func TestAdvanceRetiredGeneration(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()

	cr := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: "hcp"},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps", RetiredGeneration: 2},
	}
	g.Expect(c.Create(ctx, cr)).To(Succeed())

	// Advances forward.
	g.Expect(advanceRetiredGeneration(ctx, c, cr, 5)).To(Succeed())
	g.Expect(cr.Spec.RetiredGeneration).To(Equal(int64(5)))

	// Does not move backward.
	g.Expect(advanceRetiredGeneration(ctx, c, cr, 3)).To(Succeed())
	fetched := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), fetched)).To(Succeed())
	g.Expect(fetched.Spec.RetiredGeneration).To(Equal(int64(5)))
}

// TestAdvanceRetiredGenerationStaleInput guards against finding #2: the monotonic contract
// must be enforced against the server value, not a possibly-stale in-memory object, so a
// stale caller cannot regress retiredGeneration (which would resurrect an already-freed token).
func TestAdvanceRetiredGenerationStaleInput(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()

	cr := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: "hcp"},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps", RetiredGeneration: 2},
	}
	g.Expect(c.Create(ctx, cr)).To(Succeed())
	g.Expect(advanceRetiredGeneration(ctx, c, cr, 5)).To(Succeed())

	// A stale caller holds an old view (retiredGeneration=2) while the server is at 5.
	stale := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: "hcp"},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps", RetiredGeneration: 2},
	}
	g.Expect(advanceRetiredGeneration(ctx, c, stale, 3)).To(Succeed())

	fetched := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), fetched)).To(Succeed())
	g.Expect(fetched.Spec.RetiredGeneration).To(Equal(int64(5)))
}

func TestReconcileIgnitionPayloadConsumer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	nodePool := testNodePool("np-1")
	nodePool.Spec.Management.UpgradeType = hyperv1.UpgradeTypeInPlace
	hc := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: "clusters"},
		Spec:       hyperv1.HostedClusterSpec{PullSecret: corev1.LocalObjectReference{Name: "ps"}},
	}
	hcpNS := pkgmanifests.HostedControlPlaneNamespace("clusters", "hc")

	// The PayloadController has already generated a payload: pre-seed the CR status
	// and the store entry its token points at.
	seedCR := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps"},
	}
	c := fake.NewClientBuilder().WithScheme(api.Scheme).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).
		WithObjects(seedCR).Build()
	seedCR.Status.Current = hyperv1.PayloadReference{Token: "tok-abc", ConfigHash: "cfg1", RolloutHash: "roll1", Generation: 1}
	g.Expect(c.Status().Update(ctx, seedCR)).To(Succeed())

	store := payloadstore.NewMemStore()
	g.Expect(store.Put(ctx, payloadstore.OwnerRef{Namespace: hcpNS, Name: "np-1"}, "tok-abc", "id-1", []byte("PAYLOAD"))).To(Succeed())

	in := ignitionConsumerInputs{
		releaseVersion: "4.23.0",
		osStream:       "rhel-9",
		userConfigs:    []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "user-mc", Namespace: "clusters"}, Data: map[string]string{TokenSecretConfigKey: "u"}}},
		haproxyRaw:     "haproxy-raw",
		caCert:         []byte("CA"),
		endpoint:       "ign.example.com",
		machineSetName: "np-1",
	}

	userDataName, err := reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(userDataName).To(Equal("user-data-np-1-roll1"))

	// CR spec authored with the classified refs + finalizer, status preserved.
	cr := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, cr)).To(Succeed())
	g.Expect(sets.New(cr.Finalizers...).Has(consumerFinalizer)).To(BeTrue())
	g.Expect(refNames(cr.Spec.RolloutConfigMaps)).To(ContainElement(userConfigCopyName("np-1", "user-mc")))
	g.Expect(refNames(cr.Spec.MgmtConfigMaps)).To(Equal([]string{haproxyConfigMapName("np-1")}))
	g.Expect(cr.Spec.RolloutGlobalConfig.Name).To(Equal(rolloutGlobalConfigMapName("np-1")))
	g.Expect(cr.Status.Current.Token).To(Equal("tok-abc"))

	// userdata Secret built from status.current.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: userDataName}, &corev1.Secret{})).To(Succeed())

	// InPlace: legacy compat Secret written for the HCCO.
	legacy := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "token-np-1-cfg1"}, legacy)).To(Succeed())
	g.Expect(string(legacy.Data["release-version"])).To(Equal("4.23.0"))
}

// TestReconcileIgnitionPayloadConsumerNoChurn guards against finding #1: a steady-state
// reconcile must not rewrite the CR (which would churn its resourceVersion and, once the
// PayloadController is wired, thrash fleet rollouts by persisting intermediate empty-ref
// specs).
func TestReconcileIgnitionPayloadConsumerNoChurn(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	nodePool := testNodePool("np-1")
	nodePool.Spec.Management.UpgradeType = hyperv1.UpgradeTypeInPlace
	hc := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: "clusters"},
		Spec:       hyperv1.HostedClusterSpec{PullSecret: corev1.LocalObjectReference{Name: "ps"}},
	}
	hcpNS := pkgmanifests.HostedControlPlaneNamespace("clusters", "hc")

	seedCR := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps"},
	}
	c := fake.NewClientBuilder().WithScheme(api.Scheme).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).
		WithObjects(seedCR).Build()
	seedCR.Status.Current = hyperv1.PayloadReference{Token: "tok-abc", ConfigHash: "cfg1", RolloutHash: "roll1", Generation: 1}
	g.Expect(c.Status().Update(ctx, seedCR)).To(Succeed())

	store := payloadstore.NewMemStore()
	g.Expect(store.Put(ctx, payloadstore.OwnerRef{Namespace: hcpNS, Name: "np-1"}, "tok-abc", "id-1", []byte("PAYLOAD"))).To(Succeed())

	in := ignitionConsumerInputs{
		releaseVersion: "4.23.0",
		userConfigs:    []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "user-mc", Namespace: "clusters"}, Data: map[string]string{TokenSecretConfigKey: "u"}}},
		haproxyRaw:     "haproxy-raw",
		machineSetName: "np-1",
	}

	_, err := reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
	g.Expect(err).ToNot(HaveOccurred())
	cr1 := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, cr1)).To(Succeed())

	_, err = reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
	g.Expect(err).ToNot(HaveOccurred())
	cr2 := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, cr2)).To(Succeed())

	g.Expect(cr2.ResourceVersion).To(Equal(cr1.ResourceVersion))
	// The persisted spec always carries the full rollout refs (never an empty intermediate).
	g.Expect(cr2.Spec.RolloutConfigMaps).ToNot(BeEmpty())
}

func TestReconcileIgnitionPayloadConsumerNoPayloadYet(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	nodePool := testNodePool("np-1")
	hc := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: "clusters"},
		Spec:       hyperv1.HostedClusterSpec{PullSecret: corev1.LocalObjectReference{Name: "ps"}},
	}
	hcpNS := pkgmanifests.HostedControlPlaneNamespace("clusters", "hc")
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithStatusSubresource(&hyperv1.IgnitionPayload{}).Build()
	store := payloadstore.NewMemStore()

	in := ignitionConsumerInputs{releaseVersion: "4.23.0", haproxyRaw: "h", machineSetName: "np-1"}
	// No status.current yet: the consumer authors the CR but emits no userdata Secret.
	userDataName, err := reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(userDataName).To(BeEmpty())
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, &hyperv1.IgnitionPayload{})).To(Succeed())
}

func TestFinalizeIgnitionPayloadConsumer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	hcpNS := "clusters-hc"

	cr := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS, Finalizers: []string{consumerFinalizer, "other/keep"}},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecretName: "ps"},
	}
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(cr).Build()

	g.Expect(finalizeIgnitionPayloadConsumer(ctx, c, hcpNS, "np-1")).To(Succeed())

	// The consumer finalizer is removed; other controllers' finalizers are left intact
	// so their own cleanup can still run.
	fetched := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, fetched)).To(Succeed())
	g.Expect(sets.New(fetched.Finalizers...).Has(consumerFinalizer)).To(BeFalse())
	g.Expect(sets.New(fetched.Finalizers...).Has("other/keep")).To(BeTrue())

	// Absent CR is a no-op.
	g.Expect(finalizeIgnitionPayloadConsumer(ctx, c, hcpNS, "missing")).To(Succeed())
}
