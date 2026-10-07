package ignitionpayloadserver

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"

	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const validIgn = `{"ignition":{"version":"3.2.0"}}`

func seedStore(t *testing.T, token string) client.Client {
	t.Helper()
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	s := payloadstore.NewSecretBackedStore(c, "hcp")
	g.Expect(s.Put(context.Background(), payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}, token, "id-1", []byte(validIgn))).To(Succeed())
	return c
}

func doGet(h http.HandlerFunc, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/ignition", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Final review #1: the detached OnServed context must carry a timeout so a slow apiserver can't
// leak unbounded goroutines.
func TestOnServedContextHasDeadline(t *testing.T) {
	g := NewWithT(t)
	withTok := seedStore(t, "tok-1")
	done := make(chan bool, 1)
	s := &Server{Namespace: "hcp", Cached: withTok, Uncached: withTok,
		OnServed: func(ctx context.Context, _ payloadstore.OwnerRef, _ string) {
			_, hasDeadline := ctx.Deadline()
			done <- hasDeadline
		}}
	g.Expect(doGet(s.HandleIgnition, "tok-1").Code).To(Equal(http.StatusOK))
	g.Eventually(done).Should(Receive(BeTrue()))
}

func TestServe(t *testing.T) {
	g := NewWithT(t)
	empty := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	withTok := seedStore(t, "tok-1")

	// (RF#1) cached MISS + uncached HIT → 200 read-through, not 511. OnServed receives owner+token.
	served := make(chan struct {
		owner payloadstore.OwnerRef
		token string
	}, 1)
	s := &Server{Namespace: "hcp", Cached: empty, Uncached: withTok,
		OnServed: func(_ context.Context, o payloadstore.OwnerRef, tk string) {
			served <- struct {
				owner payloadstore.OwnerRef
				token string
			}{o, tk}
		}}
	rec := doGet(s.HandleIgnition, "tok-1")
	g.Expect(rec.Code).To(Equal(http.StatusOK))
	g.Expect(rec.Body.String()).To(Equal(validIgn))
	g.Eventually(served).Should(Receive(Equal(struct {
		owner payloadstore.OwnerRef
		token string
	}{payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}, "tok-1"})))

	// (RF#2) both miss → 511.
	s2 := &Server{Namespace: "hcp", Cached: empty, Uncached: empty}
	g.Expect(doGet(s2.HandleIgnition, "ghost").Code).To(Equal(http.StatusNetworkAuthenticationRequired))

	// cached HIT → 200 (no uncached needed).
	s3 := &Server{Namespace: "hcp", Cached: withTok, Uncached: empty}
	g.Expect(doGet(s3.HandleIgnition, "tok-1").Code).To(Equal(http.StatusOK))

	// (RF#5) missing auth → 401.
	g.Expect(doGet(s3.HandleIgnition, "").Code).To(Equal(http.StatusUnauthorized))
	// (RF#5) malformed (non-base64) token → 401.
	badReq := httptest.NewRequest(http.MethodGet, "/ignition", nil)
	badReq.Header.Set("Authorization", "Bearer !!!not-base64!!!")
	badRec := httptest.NewRecorder()
	s3.HandleIgnition(badRec, badReq)
	g.Expect(badRec.Code).To(Equal(http.StatusUnauthorized))
}
