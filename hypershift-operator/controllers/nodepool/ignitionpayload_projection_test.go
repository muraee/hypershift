package nodepool

import (
	"context"
	"sort"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func refNames(refs []hyperv1.ConfigMapReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// TestProjectConfigs pins the projection + classification rules: user configs are
// COPIED into the HCP namespace as CR-owned ConfigMaps and classified rollout; core
// and NTO configs are referenced IN PLACE (not copied, not owned); HAProxy is
// materialized CR-owned and classified mgmt; rolloutGlobalConfig is authored CR-owned
// and returned separately (it is a dedicated spec field, not a rolloutConfigMaps entry).
func TestProjectConfigs(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const hcpNS = "hcp"

	owner := &hyperv1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1", Namespace: hcpNS, UID: "uid-1"},
	}
	userConfigs := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: "user-mc", Namespace: "clusters"}, Data: map[string]string{TokenSecretConfigKey: "user-data"}},
	}
	coreConfigs := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: "core-cfg", Namespace: hcpNS}, Data: map[string]string{TokenSecretConfigKey: "core-data"}},
	}
	ntoConfigs := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: "nto-cfg", Namespace: hcpNS}, Data: map[string]string{TokenSecretConfigKey: "nto-data"}},
	}

	c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()

	rolloutRefs, mgmtRefs, globalName, err := projectConfigs(ctx, c, hcpNS, owner,
		userConfigs, coreConfigs, ntoConfigs, "haproxy-raw", []byte("global-bytes"))
	g.Expect(err).ToNot(HaveOccurred())

	userCopy := userConfigCopyName("np-1", "user-mc")
	haproxyName := haproxyConfigMapName("np-1")

	// (RF#1) classification: user + core + nto are rollout; HAProxy is mgmt.
	g.Expect(refNames(rolloutRefs)).To(Equal([]string{"core-cfg", "nto-cfg", userCopy}))
	g.Expect(refNames(mgmtRefs)).To(Equal([]string{haproxyName}))
	// rolloutGlobalConfig is a dedicated spec field, not a rolloutConfigMaps entry.
	g.Expect(globalName).To(Equal(rolloutGlobalConfigMapName("np-1")))
	g.Expect(refNames(rolloutRefs)).ToNot(ContainElement(globalName))

	assertOwnedCM := func(name, wantData string) {
		cm := &corev1.ConfigMap{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: name}, cm)).To(Succeed())
		g.Expect(cm.Data[TokenSecretConfigKey]).To(Equal(wantData))
		g.Expect(cm.OwnerReferences).To(HaveLen(1))
		g.Expect(cm.OwnerReferences[0].Name).To(Equal("np-1"))
		g.Expect(cm.OwnerReferences[0].Controller).ToNot(BeNil())
		g.Expect(*cm.OwnerReferences[0].Controller).To(BeTrue())
	}

	// (RF#2) user config COPIED into HCP ns, CR-owned, with its data.
	assertOwnedCM(userCopy, "user-data")
	// HAProxy materialized CR-owned (mgmt).
	assertOwnedCM(haproxyName, "haproxy-raw")
	// rolloutGlobalConfig authored CR-owned.
	assertOwnedCM(globalName, "global-bytes")

	// (RF#2) core and NTO are referenced IN PLACE: projectConfigs must NOT create
	// copies of them (they already live in the HCP namespace, owned elsewhere).
	notFound := func(name string) {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, client.ObjectKey{Namespace: hcpNS, Name: name}, cm)
		g.Expect(err).To(HaveOccurred())
	}
	notFound("core-cfg")
	notFound("nto-cfg")
}
