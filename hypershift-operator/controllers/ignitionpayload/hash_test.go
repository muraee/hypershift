package ignitionpayload

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestHashes(t *testing.T) {
	g := NewWithT(t)
	base := hashInputs{
		resolved:            ResolvedConfig{RolloutManifests: "ROLLOUT", MgmtManifests: "HAPROXY-v1"},
		releaseVersion:      "4.20.0",
		pullSecretName:      "ps",
		pullSecretContent:   []byte("psc"),
		trustBundleName:     "tb",
		trustBundleContent:  []byte("tbc"),
		rolloutGlobalConfig: []byte("gc"),
		osStream:            "rhel-9",
	}
	id0, ro0 := payloadIdentityHash(base), rolloutHash(base)
	g.Expect(id0).ToNot(BeEmpty())
	g.Expect(ro0).ToNot(BeEmpty())

	// (RF#1) mgmt (HAProxy) change: identity CHANGES, rollout UNCHANGED.
	m := base
	m.resolved.MgmtManifests = "HAPROXY-v2"
	g.Expect(payloadIdentityHash(m)).ToNot(Equal(id0), "mgmt change must change identity")
	g.Expect(rolloutHash(m)).To(Equal(ro0), "mgmt change must NOT change rollout hash")

	// rollout config change: BOTH change.
	r := base
	r.resolved.RolloutManifests = "ROLLOUT2"
	g.Expect(payloadIdentityHash(r)).ToNot(Equal(id0))
	g.Expect(rolloutHash(r)).ToNot(Equal(ro0))

	// (RF#2) pull-secret CONTENT rotation: identity CHANGES, rollout UNCHANGED (uses name).
	p := base
	p.pullSecretContent = []byte("psc-rotated")
	g.Expect(payloadIdentityHash(p)).ToNot(Equal(id0))
	g.Expect(rolloutHash(p)).To(Equal(ro0))
	// pull-secret NAME change: rollout CHANGES.
	pn := base
	pn.pullSecretName = "ps2"
	g.Expect(rolloutHash(pn)).ToNot(Equal(ro0))

	// trust-bundle content rotation: identity changes, rollout unchanged.
	t1 := base
	t1.trustBundleContent = []byte("tbc2")
	g.Expect(payloadIdentityHash(t1)).ToNot(Equal(id0))
	g.Expect(rolloutHash(t1)).To(Equal(ro0))

	// (RF#4) full-config (MCS gate) change: identity CHANGES (payload refreshes), rollout UNCHANGED
	// (old rollout trigger used only the proxy/image/TLS subset, not the full config hash).
	hc := base
	hc.hcConfigHash = "cfg-hash-v2"
	g.Expect(payloadIdentityHash(hc)).ToNot(Equal(id0), "full-config change must change identity")
	g.Expect(rolloutHash(hc)).To(Equal(ro0), "full-config change must NOT change rollout hash")

	// (RF#3) cloud-config change: identity CHANGES (payload refreshes), rollout UNCHANGED
	// (old Hash() excluded cloud config from the rollout trigger).
	cc := base
	cc.cloudConfigHash = "cloud-hash-v2"
	g.Expect(payloadIdentityHash(cc)).ToNot(Equal(id0), "cloud-config change must change identity")
	g.Expect(rolloutHash(cc)).To(Equal(ro0), "cloud-config change must NOT change rollout hash")

	// rolloutGlobalConfig / osStream / releaseVersion changes: rollout CHANGES.
	gc := base
	gc.rolloutGlobalConfig = []byte("gc2")
	g.Expect(rolloutHash(gc)).ToNot(Equal(ro0))
	os := base
	os.osStream = "rhel-10"
	g.Expect(rolloutHash(os)).ToNot(Equal(ro0))
	rv := base
	rv.releaseVersion = "4.21.0"
	g.Expect(rolloutHash(rv)).ToNot(Equal(ro0))
}
