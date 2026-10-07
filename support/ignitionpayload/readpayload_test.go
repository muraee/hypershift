package ignitionpayload

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReadPayload(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	owner := OwnerRef{Namespace: "hcp", Name: "np-1"}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	// Seed via the store write path, then read via the bare reader.
	s := NewSecretBackedStore(c, "hcp")
	g.Expect(s.Put(ctx, owner, "tok-1", "id-1", []byte("PAYLOAD"))).To(Succeed())

	got, gotOwner, err := ReadPayload(ctx, c, "hcp", "tok-1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal([]byte("PAYLOAD")))
	g.Expect(gotOwner).To(Equal(owner))

	_, _, err = ReadPayload(ctx, c, "hcp", "missing")
	g.Expect(err).To(MatchError(ErrNotFound))
}
