package nodepool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	supportutil "github.com/openshift/hypershift/support/util"

	configv1 "github.com/openshift/api/config/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// userDataSecretForToken builds the userdata Secret that CAPI injects into booting
// machines. Its name keys on the rollout identity (current.RolloutHash), so it is
// stable across a Policy-A payload refresh (which keeps the same token and rolloutHash)
// and changes only on a real rollout — exactly when the CAPI re-point should fire. The
// value embeds current.Token as the Bearer credential and current.ConfigHash as the
// target config version, reusing the existing ignConfig builder.
func userDataSecretForToken(nodePool *hyperv1.NodePool, hcpNamespace, endpoint string, caCert []byte,
	proxy *configv1.Proxy, current hyperv1.PayloadReference) *corev1.Secret {
	if proxy == nil {
		// ignConfig dereferences proxy.Status; an absent proxy means no proxy settings.
		proxy = &configv1.Proxy{}
	}
	encodedCACert := base64.StdEncoding.EncodeToString(caCert)
	encodedToken := base64.StdEncoding.EncodeToString([]byte(current.Token))
	cfg := ignConfig(encodedCACert, encodedToken, endpoint, current.ConfigHash, proxy, nodePool)
	value, err := json.Marshal(cfg)
	if err != nil {
		// ignConfig produces a fixed, always-serializable structure; a marshal error
		// here is not reachable with valid inputs.
		value = []byte("{}")
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", UserDataSecrePrefix, nodePool.GetName(), current.RolloutHash),
			Namespace: hcpNamespace,
		},
		Data: map[string][]byte{
			"disableTemplating": []byte(base64.StdEncoding.EncodeToString([]byte("true"))),
			"value":             value,
		},
	}
}

// reconcileLegacyInPlaceSecret keeps the unmodified HCCO InPlaceUpgrader working during
// coexistence. For InPlace NodePools it writes a token-{machineSet}-{configHash} Secret
// in the HCP namespace carrying the rendered payload (fetched from the store by
// current.Token, compressed+encoded) under "payload" and the release version under
// "release-version" — both required by the InPlaceUpgrader. It is a no-op for Replace
// NodePools, whose rollout is driven by the userdata Secret, not the token Secret.
func reconcileLegacyInPlaceSecret(ctx context.Context, c client.Client, store payloadstore.PayloadStore,
	hcpNamespace, machineSetName string, upgradeType hyperv1.UpgradeType, releaseVersion string,
	current hyperv1.PayloadReference) error {
	if upgradeType != hyperv1.UpgradeTypeInPlace {
		return nil
	}

	payload, _, err := store.Get(ctx, current.Token)
	if err != nil {
		return fmt.Errorf("failed to read payload for in-place token %q: %w", current.Token, err)
	}
	compressed, err := supportutil.CompressAndEncode(payload)
	if err != nil {
		return fmt.Errorf("failed to compress in-place payload: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", TokenSecretPrefix, machineSetName, current.ConfigHash),
			Namespace: hcpNamespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["payload"] = compressed.Bytes()
		secret.Data["release-version"] = []byte(releaseVersion)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile legacy in-place token Secret %s/%s: %w", hcpNamespace, secret.Name, err)
	}
	return nil
}
