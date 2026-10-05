package nodepool

import (
	"context"
	"fmt"
	"sort"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/netutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// userConfigCopyName is the deterministic, per-CR name of a copied user ConfigMap
// in the HCP namespace. It is namespaced by the CR name so copies for different
// NodePools sharing one HCP namespace never collide, mirroring the NTO mirror
// naming convention.
func userConfigCopyName(crName, sourceName string) string {
	return netutil.ShortenName(sourceName, crName, validation.DNS1123SubdomainMaxLength)
}

// haproxyConfigMapName is the per-CR name of the materialized apiserver-HAProxy
// ConfigMap in the HCP namespace.
func haproxyConfigMapName(crName string) string {
	return netutil.ShortenName("apiserver-haproxy", crName, validation.DNS1123SubdomainMaxLength)
}

// rolloutGlobalConfigMapName is the per-CR name of the authored rolloutGlobalConfig
// ConfigMap in the HCP namespace.
func rolloutGlobalConfigMapName(crName string) string {
	return netutil.ShortenName("rollout-global-config", crName, validation.DNS1123SubdomainMaxLength)
}

// projectConfigs writes the CR-owned projected ConfigMaps into hcpNamespace and
// returns the classified reference lists plus the rolloutGlobalConfig ConfigMap name.
//
//   - user configs are COPIED (they live in the NodePool namespace) and classified rollout;
//   - core and NTO configs are referenced IN PLACE (already in the HCP namespace, owned
//     elsewhere) and classified rollout — they are not copied and not owned by the CR;
//   - the HAProxy raw config is materialized CR-owned and classified mgmt, so a
//     management-side HAProxy bump does not churn the rollout hash;
//   - the rolloutGlobalConfig bytes are authored into a CR-owned ConfigMap whose name is
//     returned separately (it is referenced by the dedicated spec.rolloutGlobalConfig
//     field, not by rolloutConfigMaps).
//
// owner is the IgnitionPayload CR (in hcpNamespace) used as the controller owner of
// every copied/authored ConfigMap, so they cascade-delete with the CR.
func projectConfigs(ctx context.Context, c client.Client, hcpNamespace string, owner *hyperv1.IgnitionPayload,
	userConfigs, coreConfigs, ntoConfigs []corev1.ConfigMap, haproxyRaw string, rolloutGlobalConfig []byte,
) (rolloutRefs, mgmtRefs []hyperv1.ConfigMapReference, rolloutGlobalConfigName string, err error) {
	crName := owner.GetName()

	// 1. User configs: copy into the HCP namespace, CR-owned.
	for i := range userConfigs {
		src := &userConfigs[i]
		name := userConfigCopyName(crName, src.GetName())
		if err := upsertOwnedConfigMap(ctx, c, hcpNamespace, name, owner, src.Data); err != nil {
			return nil, nil, "", err
		}
		rolloutRefs = append(rolloutRefs, hyperv1.ConfigMapReference{Name: name})
	}

	// 2. & 3. Core and NTO configs: reference in place.
	for i := range coreConfigs {
		rolloutRefs = append(rolloutRefs, hyperv1.ConfigMapReference{Name: coreConfigs[i].GetName()})
	}
	for i := range ntoConfigs {
		rolloutRefs = append(rolloutRefs, hyperv1.ConfigMapReference{Name: ntoConfigs[i].GetName()})
	}

	// 4. rolloutGlobalConfig: author CR-owned ConfigMap, referenced via the dedicated
	// spec field (returned name), not via rolloutConfigMaps.
	rolloutGlobalConfigName = rolloutGlobalConfigMapName(crName)
	if err := upsertOwnedConfigMap(ctx, c, hcpNamespace, rolloutGlobalConfigName, owner,
		map[string]string{TokenSecretConfigKey: string(rolloutGlobalConfig)}); err != nil {
		return nil, nil, "", err
	}

	// 5. HAProxy: materialize CR-owned ConfigMap, classified mgmt.
	haproxyName := haproxyConfigMapName(crName)
	if err := upsertOwnedConfigMap(ctx, c, hcpNamespace, haproxyName, owner,
		map[string]string{TokenSecretConfigKey: haproxyRaw}); err != nil {
		return nil, nil, "", err
	}
	mgmtRefs = append(mgmtRefs, hyperv1.ConfigMapReference{Name: haproxyName})

	// Deterministic ordering so the authored spec is stable across reconciles.
	sortRefs(rolloutRefs)
	sortRefs(mgmtRefs)
	return rolloutRefs, mgmtRefs, rolloutGlobalConfigName, nil
}

func sortRefs(refs []hyperv1.ConfigMapReference) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
}

// upsertOwnedConfigMap creates or updates a ConfigMap in hcpNamespace with the given
// data and sets owner as its controller reference.
func upsertOwnedConfigMap(ctx context.Context, c client.Client, hcpNamespace, name string,
	owner *hyperv1.IgnitionPayload, data map[string]string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hcpNamespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		if err := controllerutil.SetControllerReference(owner, cm, api.Scheme); err != nil {
			return err
		}
		cm.Data = data
		return nil
	}); err != nil {
		return fmt.Errorf("failed to project ConfigMap %s/%s: %w", hcpNamespace, name, err)
	}
	return nil
}
