package nodepool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ignitionapi "github.com/coreos/ignition/v2/config/v3_2/types"
)

func httpHeaderValue(cfg ignitionapi.Config, name string) string {
	for _, m := range cfg.Ignition.Config.Merge {
		for _, h := range m.HTTPHeaders {
			if h.Name == name && h.Value != nil {
				return *h.Value
			}
		}
	}
	return ""
}

func TestUserDataSecretForToken(t *testing.T) {
	g := NewWithT(t)
	nodePool := testNodePool("np-1")
	current := hyperv1.PayloadReference{Token: "tok-abc", ConfigHash: "cfg1", RolloutHash: "roll1", Generation: 1}

	secret := userDataSecretForToken(nodePool, "hcp", "ign.example.com", []byte("CA-CERT"), nil, current)

	// Name keys on the rollout identity; namespace is the HCP namespace.
	g.Expect(secret.Name).To(Equal("user-data-np-1-roll1"))
	g.Expect(secret.Namespace).To(Equal("hcp"))

	// Two keys, matching the existing userdata Secret structure.
	g.Expect(secret.Data).To(HaveKey("disableTemplating"))
	g.Expect(string(secret.Data["disableTemplating"])).To(Equal(base64.StdEncoding.EncodeToString([]byte("true"))))

	var cfg ignitionapi.Config
	g.Expect(json.Unmarshal(secret.Data["value"], &cfg)).To(Succeed())
	// The Bearer token embeds status.current.token (base64), and the header carries
	// the payload-identity hash as the target config version.
	g.Expect(httpHeaderValue(cfg, "Authorization")).To(Equal("Bearer " + base64.StdEncoding.EncodeToString([]byte("tok-abc"))))
	g.Expect(httpHeaderValue(cfg, "TargetConfigVersionHash")).To(Equal("cfg1"))

	// RF#5: the name is stable across a Policy-A refresh (token/configHash change, same
	// rolloutHash) and changes only when the rollout identity changes.
	refreshed := userDataSecretForToken(nodePool, "hcp", "ign.example.com", []byte("CA-CERT"), nil,
		hyperv1.PayloadReference{Token: "tok-xyz", ConfigHash: "cfg2", RolloutHash: "roll1", Generation: 1})
	g.Expect(refreshed.Name).To(Equal(secret.Name))

	rolled := userDataSecretForToken(nodePool, "hcp", "ign.example.com", []byte("CA-CERT"), nil,
		hyperv1.PayloadReference{Token: "tok-xyz", ConfigHash: "cfg2", RolloutHash: "roll2", Generation: 2})
	g.Expect(rolled.Name).ToNot(Equal(secret.Name))
}

func TestReconcileLegacyInPlaceSecret(t *testing.T) {
	ctx := context.Background()
	current := hyperv1.PayloadReference{Token: "tok-abc", ConfigHash: "cfg1", RolloutHash: "roll1", Generation: 1}

	t.Run("InPlace writes a byte-compatible token Secret", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
		store := payloadstore.NewMemStore()
		g.Expect(store.Put(ctx, payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}, "tok-abc", "id-1", []byte("PAYLOAD"))).To(Succeed())

		err := reconcileLegacyInPlaceSecret(ctx, c, store, "hcp", "ms-1", hyperv1.UpgradeTypeInPlace, "4.23.0", current)
		g.Expect(err).ToNot(HaveOccurred())

		secret := &corev1.Secret{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: "hcp", Name: "token-ms-1-cfg1"}, secret)).To(Succeed())

		wantPayload, err := supportutil.CompressAndEncode([]byte("PAYLOAD"))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(secret.Data["payload"]).To(Equal(wantPayload.Bytes()))
		// The HCCO InPlaceUpgrader errors without a non-empty release-version key.
		g.Expect(string(secret.Data["release-version"])).To(Equal("4.23.0"))
	})

	t.Run("Replace writes nothing", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
		store := payloadstore.NewMemStore()
		g.Expect(store.Put(ctx, payloadstore.OwnerRef{Namespace: "hcp", Name: "np-1"}, "tok-abc", "id-1", []byte("PAYLOAD"))).To(Succeed())

		err := reconcileLegacyInPlaceSecret(ctx, c, store, "hcp", "ms-1", hyperv1.UpgradeTypeReplace, "4.23.0", current)
		g.Expect(err).ToNot(HaveOccurred())

		secret := &corev1.Secret{}
		err = c.Get(ctx, client.ObjectKey{Namespace: "hcp", Name: "token-ms-1-cfg1"}, secret)
		g.Expect(err).To(HaveOccurred())
	})
}
