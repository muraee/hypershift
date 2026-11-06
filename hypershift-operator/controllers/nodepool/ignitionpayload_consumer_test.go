package nodepool

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	configv1 "github.com/openshift/api/config/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	capiv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestIgnitionPayloadConsumerEnabled is serial (no t.Parallel): it mutates the global management
// feature gate and restores it on return. Top-level tests run sequentially, so the flip is not observed
// by other tests.
func TestIgnitionPayloadConsumerEnabled(t *testing.T) {
	g := NewWithT(t)
	defer featuregate.ConfigureFeatureSet(string(configv1.Default))

	hc := &hyperv1.HostedCluster{}
	disabled := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"}},
	}

	featuregate.ConfigureFeatureSet(string(configv1.Default))
	g.Expect(ignitionPayloadConsumerEnabled(hc, true)).To(BeFalse(), "gate off ⇒ legacy path")

	featuregate.ConfigureFeatureSet(string(configv1.TechPreviewNoUpgrade))
	g.Expect(ignitionPayloadConsumerEnabled(hc, true)).To(BeTrue(), "gate on + CPO supports split ignition ⇒ new path")
	g.Expect(ignitionPayloadConsumerEnabled(hc, false)).To(BeFalse(), "gate on but CPO lacks split-ignition support ⇒ legacy path")
	g.Expect(ignitionPayloadConsumerEnabled(disabled, true)).To(BeFalse(), "ignition disabled on the HC ⇒ legacy path")
}

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

func TestAdvanceRetiredRolloutGeneration(t *testing.T) {
	ctx := context.Background()

	newCR := func() *hyperv1.IgnitionPayload {
		return &hyperv1.IgnitionPayload{
			ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: "hcp"},
			Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecret: hyperv1.SecretReference{Name: "ps"}, RetiredRolloutGeneration: 2},
		}
	}

	t.Run("advances forward but never backward", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
		cr := newCR()
		g.Expect(c.Create(ctx, cr)).To(Succeed())

		g.Expect(advanceRetiredRolloutGeneration(ctx, c, cr, 5)).To(Succeed())
		g.Expect(cr.Spec.RetiredRolloutGeneration).To(Equal(int64(5)))

		g.Expect(advanceRetiredRolloutGeneration(ctx, c, cr, 3)).To(Succeed())
		fetched := &hyperv1.IgnitionPayload{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), fetched)).To(Succeed())
		g.Expect(fetched.Spec.RetiredRolloutGeneration).To(Equal(int64(5)))
	})

	// The monotonic contract must be enforced against the server value, not a possibly-stale
	// in-memory object, so a stale caller cannot regress retiredRolloutGeneration (which would
	// resurrect an already-freed payload).
	t.Run("a stale caller cannot regress the server value", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
		cr := newCR()
		g.Expect(c.Create(ctx, cr)).To(Succeed())
		g.Expect(advanceRetiredRolloutGeneration(ctx, c, cr, 5)).To(Succeed())

		stale := newCR() // old view at retiredRolloutGeneration=2
		g.Expect(advanceRetiredRolloutGeneration(ctx, c, stale, 3)).To(Succeed())

		fetched := &hyperv1.IgnitionPayload{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cr), fetched)).To(Succeed())
		g.Expect(fetched.Spec.RetiredRolloutGeneration).To(Equal(int64(5)))
	})
}

func TestPreviousGenerationDrained(t *testing.T) {
	ctx := context.Background()
	const ns, np, current = "hcp", "np-1", "np-1-2"

	// md builds a Replace MachineDeployment pointed at dataSecret with the given rollout counts.
	md := func(dataSecret string, gen, observed int64, replicas, upToDate, available int32) *capiv1.MachineDeployment {
		return &capiv1.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: np, Namespace: ns, Generation: gen},
			Spec: capiv1.MachineDeploymentSpec{
				Replicas: ptr.To[int32](2),
				Template: capiv1.MachineTemplateSpec{Spec: capiv1.MachineSpec{
					Bootstrap: capiv1.Bootstrap{DataSecretName: ptr.To(dataSecret)},
				}},
			},
			Status: capiv1.MachineDeploymentStatus{
				ObservedGeneration: observed,
				Replicas:           ptr.To(replicas),
				UpToDateReplicas:   ptr.To(upToDate),
				AvailableReplicas:  ptr.To(available),
			},
		}
	}
	check := func(objs []client.Object, upgradeType hyperv1.UpgradeType) (bool, error) {
		c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(objs...).Build()
		return previousGenerationDrained(ctx, c, ns, np, upgradeType, current)
	}

	t.Run("Replace fully rolled out onto current ⇒ drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check([]client.Object{md(current, 1, 1, 2, 2, 2)}, hyperv1.UpgradeTypeReplace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeTrue())
	})
	t.Run("Replace not yet fully up-to-date ⇒ not drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check([]client.Object{md(current, 1, 1, 2, 1, 2)}, hyperv1.UpgradeTypeReplace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeFalse())
	})
	t.Run("Replace status lagging spec generation ⇒ not drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check([]client.Object{md(current, 2, 1, 2, 2, 2)}, hyperv1.UpgradeTypeReplace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeFalse())
	})
	t.Run("Replace MD still on previous userData ⇒ not drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check([]client.Object{md("np-1-1", 1, 1, 2, 2, 2)}, hyperv1.UpgradeTypeReplace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeFalse())
	})
	t.Run("Replace MD absent ⇒ not drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check(nil, hyperv1.UpgradeTypeReplace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeFalse())
	})
	t.Run("InPlace is out of scope ⇒ not drained", func(t *testing.T) {
		g := NewWithT(t)
		drained, err := check([]client.Object{md(current, 1, 1, 2, 2, 2)}, hyperv1.UpgradeTypeInPlace)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(drained).To(BeFalse())
	})
}

// seedConsumerCR creates a CR whose generator has already published status.current, plus the
// matching store entry, so the consumer end-to-end path has a payload to reference.
func seedConsumerCR(ctx context.Context, g *WithT, hcpNS string) (client.Client, payloadstore.PayloadStore) {
	seedCR := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecret: hyperv1.SecretReference{Name: "ps"}},
	}
	c := fake.NewClientBuilder().WithScheme(api.Scheme).
		WithStatusSubresource(&hyperv1.IgnitionPayload{}).
		WithObjects(seedCR).Build()
	seedCR.Status.Current = hyperv1.PayloadReference{ConfigHash: "cfg1", RolloutHash: "roll1", UserDataSecret: hyperv1.SecretReference{Name: "np-1-1"}, RolloutGeneration: 1}
	g.Expect(c.Status().Update(ctx, seedCR)).To(Succeed())

	store := payloadstore.NewMemStore()
	g.Expect(store.Put(ctx, payloadstore.OwnerRef{Namespace: hcpNS, Name: "np-1"}, 1, "cfg1", []byte("PAYLOAD"))).To(Succeed())
	return c, store
}

func TestReconcileIgnitionPayloadConsumer(t *testing.T) {
	hc := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: "clusters"},
		Spec:       hyperv1.HostedClusterSpec{PullSecret: corev1.LocalObjectReference{Name: "ps"}},
	}
	hcpNS := pkgmanifests.HostedControlPlaneNamespace("clusters", "hc")

	t.Run("references the generator-authored userData Secret and writes the InPlace compat Secret", func(t *testing.T) {
		g := NewWithT(t)
		ctx := context.Background()
		nodePool := testNodePool("np-1")
		nodePool.Spec.Management.UpgradeType = hyperv1.UpgradeTypeInPlace
		c, store := seedConsumerCR(ctx, g, hcpNS)

		in := ignitionConsumerInputs{
			releaseVersion:          "4.23.0",
			osStream:                "rhel-9",
			additionalRolloutInputs: "private=false,port=6443",
			userConfigs:             []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "user-mc", Namespace: "clusters"}, Data: map[string]string{TokenSecretConfigKey: "u"}}},
			haproxyRaw:              "haproxy-raw",
			machineSetName:          "np-1",
		}

		// A never-provisioned pool (no existing DataSecretName) is born on the generator's canonical
		// userData Secret name from status.current; the consumer never mints userdata itself.
		decision, err := reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(decision.userDataSecretName).To(Equal("np-1-1"))

		// CR spec authored with the classified refs + endpoint rollout input + finalizer; status
		// preserved.
		cr := &hyperv1.IgnitionPayload{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, cr)).To(Succeed())
		g.Expect(sets.New(cr.Finalizers...).Has(consumerFinalizer)).To(BeTrue())
		g.Expect(refNames(cr.Spec.RolloutConfigMaps)).To(ContainElement(userConfigCopyName("np-1", "user-mc")))
		g.Expect(refNames(cr.Spec.ManagementConfigMaps)).To(Equal([]string{haproxyConfigMapName("np-1")}))
		g.Expect(cr.Spec.RolloutGlobalConfig.Name).To(Equal(rolloutGlobalConfigMapName("np-1")))
		g.Expect(cr.Spec.AdditionalRolloutInputs).To(Equal("private=false,port=6443"))
		g.Expect(cr.Status.Current.UserDataSecret.Name).To(Equal("np-1-1"))

		// InPlace: legacy compat Secret written for the HCCO (name keyed by configHash).
		legacy := &corev1.Secret{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "token-np-1-cfg1"}, legacy)).To(Succeed())
		g.Expect(string(legacy.Data["release-version"])).To(Equal("4.23.0"))
	})

	// A steady-state reconcile must not rewrite the CR (which would churn its resourceVersion and,
	// once wired, thrash fleet rollouts by persisting intermediate empty-ref specs).
	t.Run("a steady-state reconcile does not churn the CR", func(t *testing.T) {
		g := NewWithT(t)
		ctx := context.Background()
		nodePool := testNodePool("np-1")
		nodePool.Spec.Management.UpgradeType = hyperv1.UpgradeTypeInPlace
		c, store := seedConsumerCR(ctx, g, hcpNS)

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
	})

	t.Run("no userData Secret name until the generator publishes status.current", func(t *testing.T) {
		g := NewWithT(t)
		ctx := context.Background()
		nodePool := testNodePool("np-1")
		c := fake.NewClientBuilder().WithScheme(api.Scheme).WithStatusSubresource(&hyperv1.IgnitionPayload{}).Build()
		store := payloadstore.NewMemStore()

		in := ignitionConsumerInputs{releaseVersion: "4.23.0", haproxyRaw: "h", machineSetName: "np-1"}
		// No status.current yet: the consumer authors the CR but returns the zero adoption decision.
		decision, err := reconcileIgnitionPayloadConsumer(ctx, c, store, nodePool, hc, in)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(decision.userDataSecretName).To(BeEmpty())
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, &hyperv1.IgnitionPayload{})).To(Succeed())
	})
}

func TestFinalizeIgnitionPayloadConsumer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	hcpNS := "clusters-hc"

	cr := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS, Finalizers: []string{consumerFinalizer, "other/keep"}},
		Spec:       hyperv1.IgnitionPayloadSpec{ReleaseImage: "img", PullSecret: hyperv1.SecretReference{Name: "ps"}},
	}
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(cr).Build()

	g.Expect(finalizeIgnitionPayloadConsumer(ctx, c, hcpNS, "np-1")).To(Succeed())

	// The consumer finalizer is removed; other controllers' finalizers are left intact so their own
	// cleanup can still run.
	fetched := &hyperv1.IgnitionPayload{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: "np-1"}, fetched)).To(Succeed())
	g.Expect(sets.New(fetched.Finalizers...).Has(consumerFinalizer)).To(BeFalse())
	g.Expect(sets.New(fetched.Finalizers...).Has("other/keep")).To(BeTrue())

	// Absent CR is a no-op.
	g.Expect(finalizeIgnitionPayloadConsumer(ctx, c, hcpNS, "missing")).To(Succeed())
}
