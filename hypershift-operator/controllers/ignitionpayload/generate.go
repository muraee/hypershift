package ignitionpayload

import (
	"context"
	"errors"

	ignserver "github.com/openshift/hypershift/ignition-server/controllers"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
)

// genInputs are the arguments assembled for the MCO pipeline's GetPayload call.
type genInputs struct {
	releaseImage        string
	customConfig        string
	pullSecretHash      string
	trustBundleHash     string
	hcConfigurationHash string
	osStream            string
	cloudConfigHash     string
}

// payloadGenerator renders ignition payloads via the MCO pipeline and stores them in the
// PayloadStore, idempotently across leader failover.
type payloadGenerator struct {
	store    payloadstore.PayloadStore
	provider ignserver.IgnitionProvider
}

// ensurePayload returns the token for a payload of the given payload-identity hash, generating
// and storing it only when the store has no entry for (owner, identityHash). On a hit it reuses
// the existing token and skips the release-image pull (one pull per config version). newToken is
// the UUID used when a fresh entry is created.
func (g *payloadGenerator) ensurePayload(ctx context.Context, owner payloadstore.OwnerRef, identityHash, newToken string, in genInputs) (token string, generated bool, err error) {
	existing, err := g.store.FindByIdentity(ctx, owner, identityHash)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, payloadstore.ErrNotFound) {
		return "", false, err
	}

	payload, err := g.render(ctx, in)
	if err != nil {
		return "", false, err
	}
	if err := g.store.Put(ctx, owner, newToken, identityHash, payload); err != nil {
		return "", false, err
	}
	return newToken, true, nil
}

// refreshPayload re-renders and overwrites the bytes behind an EXISTING token in place
// (the Policy A refresh path), updating the stored identity label to the new identityHash.
func (g *payloadGenerator) refreshPayload(ctx context.Context, owner payloadstore.OwnerRef, token, identityHash string, in genInputs) error {
	payload, err := g.render(ctx, in)
	if err != nil {
		return err
	}
	return g.store.Put(ctx, owner, token, identityHash, payload)
}

func (g *payloadGenerator) render(ctx context.Context, in genInputs) ([]byte, error) {
	return g.provider.GetPayload(ctx, in.releaseImage, in.customConfig, in.pullSecretHash,
		in.trustBundleHash, in.hcConfigurationHash, in.osStream, in.cloudConfigHash)
}
