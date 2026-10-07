package ignitionpayload

import (
	supportutil "github.com/openshift/hypershift/support/util"
)

// rolloutHashFormulaVersion is the version of the rollout-hash formula below. It is bumped
// BY HAND in the same change that alters which inputs feed rolloutHash. status.rolloutHashVersion
// records the version used for a CR's current.rolloutHash; when the stored version is older than
// this constant the controller rebaselines the stored hash WITHOUT advancing generation (see the
// reconcile state machine). Mirrors the versioned NodePool config hash (#9007).
const rolloutHashFormulaVersion int64 = 1

// hashInputs carries everything the two hashes need.
type hashInputs struct {
	resolved            ResolvedConfig // validated rollout + mgmt manifests
	releaseVersion      string         // releaseImage.Version()
	pullSecretName      string
	pullSecretContent   []byte
	trustBundleName     string // spec.additionalTrustBundle.Name ("" if unset)
	trustBundleContent  []byte
	rolloutGlobalConfig []byte // bytes of the rolloutGlobalConfig ConfigMap ("" if unset)
	osStream            string
}

// payloadIdentityHash covers EVERYTHING embedded in the rendered payload, including the
// pull-secret and trust-bundle CONTENTS. It labels the store entry and makes generation
// idempotent: an in-place credential/CA rotation changes this hash, so FindByIdentity never
// reuses stale bytes.
func payloadIdentityHash(in hashInputs) string {
	return supportutil.HashSimple(in.resolved.RolloutManifests +
		in.resolved.MgmtManifests +
		in.releaseVersion +
		string(in.pullSecretContent) +
		string(in.trustBundleContent) +
		string(in.rolloutGlobalConfig) +
		in.osStream)
}

// rolloutHash covers only rollout-relevant inputs. It EXCLUDES the mgmt (HAProxy) manifests
// and uses the pull-secret/trust-bundle NAMES (not their contents), so a management-side bump
// or an in-place credential rotation does not advance it. A change here triggers a node rollout.
func rolloutHash(in hashInputs) string {
	return supportutil.HashSimple(in.resolved.RolloutManifests +
		in.releaseVersion +
		in.pullSecretName +
		in.trustBundleName +
		string(in.rolloutGlobalConfig) +
		in.osStream)
}
