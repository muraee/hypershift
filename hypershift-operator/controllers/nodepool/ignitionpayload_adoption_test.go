package nodepool

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func TestResolveAdoption(t *testing.T) {
	const legacyName = "user-data-np-1-legacyhash"
	current := hyperv1.PayloadReference{UserDataSecret: hyperv1.SecretReference{Name: "np-1-3"}, RolloutGeneration: 3, ConfigHash: "cfg3"}

	t.Run("brand-new pool is born on the canonical name at the current generation", func(t *testing.T) {
		g := NewWithT(t)
		np := testNodePool("np-1")
		d := resolveAdoption(np, current, "")
		g.Expect(d.userDataSecretName).To(Equal("np-1-3"))
		g.Expect(d.inPlaceConfigVersion).To(Equal("cfg3"))
		g.Expect(d.copyContent).To(BeFalse())
		g.Expect(d.sweepInFlight).To(BeFalse())
		g.Expect(np.Annotations[nodePoolAnnotationIgnitionDeployedGeneration]).To(Equal("3"))
	})

	t.Run("first encounter of a provisioned pool adopts without a roll", func(t *testing.T) {
		g := NewWithT(t)
		np := testNodePool("np-1")
		d := resolveAdoption(np, current, legacyName)
		// Keep the live name (no DataSecretName change ⇒ CAPI does not roll); copy new content under it;
		// do NOT stamp the InPlace config version (would reboot already-joined nodes); sweep in-flight.
		g.Expect(d.userDataSecretName).To(Equal(legacyName))
		g.Expect(d.copyContent).To(BeTrue())
		g.Expect(d.sweepInFlight).To(BeTrue())
		g.Expect(d.inPlaceConfigVersion).To(BeEmpty())
		g.Expect(np.Annotations[nodePoolAnnotationIgnitionDeployedGeneration]).To(Equal("3"))
	})

	t.Run("a generation advance after adoption re-points to the canonical name", func(t *testing.T) {
		g := NewWithT(t)
		np := testNodePool("np-1")
		np.Annotations = map[string]string{nodePoolAnnotationIgnitionDeployedGeneration: "2"}
		d := resolveAdoption(np, current, legacyName)
		g.Expect(d.userDataSecretName).To(Equal("np-1-3"))
		g.Expect(d.inPlaceConfigVersion).To(Equal("cfg3"))
		g.Expect(d.copyContent).To(BeFalse())
		g.Expect(np.Annotations[nodePoolAnnotationIgnitionDeployedGeneration]).To(Equal("3"))
	})

	t.Run("steady state keeps the live name without a roll", func(t *testing.T) {
		g := NewWithT(t)
		np := testNodePool("np-1")
		np.Annotations = map[string]string{nodePoolAnnotationIgnitionDeployedGeneration: "3"}
		d := resolveAdoption(np, current, legacyName)
		g.Expect(d.userDataSecretName).To(Equal(legacyName))
		g.Expect(d.copyContent).To(BeTrue())
		g.Expect(d.inPlaceConfigVersion).To(BeEmpty())
		g.Expect(d.sweepInFlight).To(BeFalse())
	})
}
