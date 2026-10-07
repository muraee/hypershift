package ignitionpayloadserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"
)

// TestHealthz verifies the /healthz handler always returns 200, independent of TLS client auth or
// payload state, so the kubelet liveness/readiness probes can confirm the serving process is up.
func TestHealthz(t *testing.T) {
	g := NewWithT(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	HandleHealthz(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusOK))
}
