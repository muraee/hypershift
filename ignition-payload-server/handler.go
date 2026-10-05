package ignitionpayloadserver

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"regexp"

	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	supportutil "github.com/openshift/hypershift/support/util"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ignPathPattern = regexp.MustCompile("^/ignition[^/ ]*$")

// Server serves ignition payloads from the PayloadStore with read-through on cache miss.
type Server struct {
	Namespace string
	// Cached reads via the manager's informer cache (fast path).
	Cached client.Reader
	// Uncached reads directly from the API server (read-through when the cache has not yet
	// hydrated a just-published token).
	Uncached client.Reader
	// OnServed, if set, is invoked (best-effort, asynchronously) after a successful serve with the
	// owning CR and the served token, to record IgnitionReached.
	OnServed func(ctx context.Context, owner payloadstore.OwnerRef, token string)
}

// HandleIgnition serves GET /ignition. It authorizes the Bearer token, resolves the payload from
// the store (cached, then read-through to the API on a miss), serves the sanitized payload, and
// triggers the OnServed callback.
func (s *Server) HandleIgnition(w http.ResponseWriter, r *http.Request) {
	if !ignPathPattern.MatchString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	payload, owner, err := s.lookup(r.Context(), token)
	switch {
	case errors.Is(err, payloadstore.ErrNotFound):
		// 5xx so ignition backs off and retries while the payload propagates.
		// https://coreos.github.io/ignition/operator-notes/#http-backoff-and-retry
		http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
		return
	case err != nil:
		log.Printf("failed to read payload: %s", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := supportutil.SanitizeIgnitionPayload(payload); err != nil {
		log.Printf("invalid ignition payload: %s", err)
		http.Error(w, "Invalid ignition payload", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)

	if s.OnServed != nil {
		// Detach from the request context so the IgnitionReached write completes after the
		// response is written.
		go s.OnServed(context.WithoutCancel(r.Context()), owner, token)
	}
}

// lookup resolves token via the cached reader, falling back to the uncached (API) reader on a
// cache miss so a token is servable the instant the generator's Put returns.
func (s *Server) lookup(ctx context.Context, token string) ([]byte, payloadstore.OwnerRef, error) {
	payload, owner, err := payloadstore.ReadPayload(ctx, s.Cached, s.Namespace, token)
	if errors.Is(err, payloadstore.ErrNotFound) {
		return payloadstore.ReadPayload(ctx, s.Uncached, s.Namespace, token)
	}
	return payload, owner, err
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil {
		return "", false
	}
	return string(decoded), true
}
