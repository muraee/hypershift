package ignitionpayload

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
)

type fakeProvider struct {
	calls               int
	payload             []byte
	lastPullSecretHash  string
	lastTrustBundleHash string
}

func (f *fakeProvider) GetPayload(ctx context.Context, img, cfg, pullSecretHash, trustBundleHash, hcConfigHash, osStream, cloudConfigHash string) ([]byte, error) {
	f.calls++
	f.lastPullSecretHash = pullSecretHash
	f.lastTrustBundleHash = trustBundleHash
	return f.payload, nil
}

func TestEnsurePayloadIdempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fp := &fakeProvider{payload: []byte("PAYLOAD")}
	gen := &payloadGenerator{store: payloadstore.NewMemStore(), provider: fp}
	owner := payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}

	tok, generated, err := gen.ensurePayload(ctx, owner, "id-1", "tok-1", genInputs{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(generated).To(BeTrue())
	g.Expect(tok).To(Equal("tok-1"))
	g.Expect(fp.calls).To(Equal(1))

	// (RF#6) second call for the SAME identity reuses — no extra pull.
	tok2, generated2, err := gen.ensurePayload(ctx, owner, "id-1", "tok-UNUSED", genInputs{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(generated2).To(BeFalse())
	g.Expect(tok2).To(Equal("tok-1"))
	g.Expect(fp.calls).To(Equal(1))

	// refresh overwrites bytes behind the SAME token (Policy A).
	fp.payload = []byte("PAYLOAD2")
	g.Expect(gen.refreshPayload(ctx, owner, "tok-1", "id-2", genInputs{})).To(Succeed())
	got, _, err := gen.store.Get(ctx, "tok-1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal([]byte("PAYLOAD2")))
	// refresh re-rendered once more.
	g.Expect(fp.calls).To(Equal(2))
}
