package ignitionpayload

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/manifests"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func crWithRefs(name string, spec hyperv1.IgnitionPayloadSpec) *hyperv1.IgnitionPayload {
	return &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Spec:       spec,
	}
}

func reqFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNS, Name: name}}
}

func TestEnqueueForConfigMap(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	crA := crWithRefs("a", hyperv1.IgnitionPayloadSpec{RolloutConfigMaps: []hyperv1.ConfigMapReference{{Name: "user-a"}}})
	crB := crWithRefs("b", hyperv1.IgnitionPayloadSpec{
		MgmtConfigMaps:      []hyperv1.ConfigMapReference{{Name: "haproxy-b"}},
		RolloutGlobalConfig: hyperv1.ConfigMapReference{Name: "gc-b"},
	})
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(crA, crB).Build()
	r := &Reconciler{Client: c, Namespace: testNS}

	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, "user-a", ""))).To(ConsistOf(reqFor("a")))
	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, "haproxy-b", ""))).To(ConsistOf(reqFor("b")))
	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, "gc-b", ""))).To(ConsistOf(reqFor("b")))
	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, "unrelated", ""))).To(BeEmpty())
}

func TestEnqueueForCloudConfig(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	// Neither CR references the cloud-config ConfigMap by name; it is HCP-level, consumed via the
	// HCP's platform, not a CR ref.
	crA := crWithRefs("a", hyperv1.IgnitionPayloadSpec{RolloutConfigMaps: []hyperv1.ConfigMapReference{{Name: "user-a"}}})
	crB := crWithRefs("b", hyperv1.IgnitionPayloadSpec{PullSecretName: "ps-b"})
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(crA, crB).Build()
	r := &Reconciler{Client: c, Namespace: testNS}

	// A cloud-config ConfigMap change (its hash is HCP-level) enqueues every CR in the namespace.
	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, manifests.AzureProviderConfig(testNS).Name, ""))).To(ConsistOf(reqFor("a"), reqFor("b")))
	g.Expect(r.enqueueIgnitionPayloadsForConfigMap(ctx, cfgMap(testNS, manifests.OpenStackProviderConfig(testNS).Name, ""))).To(ConsistOf(reqFor("a"), reqFor("b")))
}

func TestEnqueueForHostedControlPlane(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	crA := crWithRefs("a", hyperv1.IgnitionPayloadSpec{})
	crB := crWithRefs("b", hyperv1.IgnitionPayloadSpec{})
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(crA, crB).Build()
	r := &Reconciler{Client: c, Namespace: testNS}

	// The full cluster-configuration (MCS gate) hash is HCP-level, so an HCP change must re-reconcile
	// every CR in the namespace.
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "hcp"}}
	g.Expect(r.enqueueIgnitionPayloadsForHostedControlPlane(ctx, hcp)).To(ConsistOf(reqFor("a"), reqFor("b")))
}

func TestEnqueueForSecret(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	crA := crWithRefs("a", hyperv1.IgnitionPayloadSpec{PullSecretName: "ps-a"})
	crB := crWithRefs("b", hyperv1.IgnitionPayloadSpec{PullSecretName: "ps-b"})
	c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(crA, crB).Build()
	r := &Reconciler{Client: c, Namespace: testNS}

	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}}
	}
	g.Expect(r.enqueueIgnitionPayloadsForSecret(ctx, secret("ps-a"))).To(ConsistOf(reqFor("a")))
	g.Expect(r.enqueueIgnitionPayloadsForSecret(ctx, secret("ps-b"))).To(ConsistOf(reqFor("b")))
	g.Expect(r.enqueueIgnitionPayloadsForSecret(ctx, secret("nope"))).To(BeEmpty())
}
