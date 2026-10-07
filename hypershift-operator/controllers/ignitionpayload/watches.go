package ignitionpayload

import (
	"context"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/manifests"

	corev1 "k8s.io/api/core/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SetupWithManager registers the reconciler: it owns IgnitionPayload and re-reconciles CRs when
// any input the Reconcile reads changes. That includes ConfigMaps/Secrets a CR references by name,
// the HostedControlPlane (whose Spec.Configuration drives the HCP-level full-config/MCS gate hash),
// and the platform cloud-provider config ConfigMap (also HCP-level) — the latter two affect every
// CR in the namespace, not just CRs that name them.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1.IgnitionPayload{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.enqueueIgnitionPayloadsForConfigMap)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueIgnitionPayloadsForSecret)).
		Watches(&hyperv1.HostedControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.enqueueIgnitionPayloadsForHostedControlPlane)).
		Complete(r)
}

// enqueueIgnitionPayloadsForConfigMap enqueues IgnitionPayloads in the ConfigMap's namespace that
// care about it: every CR when the ConfigMap is the platform cloud-provider config (HCP-level,
// consumed via the HCP's platform, not a CR ref), otherwise only CRs that reference it by name
// (rolloutConfigMaps, mgmtConfigMaps, rolloutGlobalConfig, or additionalTrustBundle).
func (r *Reconciler) enqueueIgnitionPayloadsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetNamespace()
	name := obj.GetName()
	isCloudConfig := name == manifests.AzureProviderConfig(ns).Name || name == manifests.OpenStackProviderConfig(ns).Name
	return r.enqueueMatching(ctx, ns, func(cr *hyperv1.IgnitionPayload) bool {
		return isCloudConfig || crReferencesConfigMap(cr, name)
	})
}

// enqueueIgnitionPayloadsForHostedControlPlane enqueues every IgnitionPayload in the HCP's
// namespace: the HCP's Spec.Configuration drives the full-config/MCS gate hash (and its platform
// selects the cloud-provider config), both HCP-level inputs shared by all CRs.
func (r *Reconciler) enqueueIgnitionPayloadsForHostedControlPlane(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.enqueueMatching(ctx, obj.GetNamespace(), func(*hyperv1.IgnitionPayload) bool {
		return true
	})
}

// enqueueIgnitionPayloadsForSecret enqueues every IgnitionPayload in the Secret's namespace that
// references it as its pull secret.
func (r *Reconciler) enqueueIgnitionPayloadsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.enqueueMatching(ctx, obj.GetNamespace(), func(cr *hyperv1.IgnitionPayload) bool {
		return cr.Spec.PullSecretName == obj.GetName()
	})
}

func (r *Reconciler) enqueueMatching(ctx context.Context, namespace string, match func(*hyperv1.IgnitionPayload) bool) []reconcile.Request {
	list := &hyperv1.IgnitionPayloadList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		cr := &list.Items[i]
		if match(cr) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		}
	}
	return reqs
}

func crReferencesConfigMap(cr *hyperv1.IgnitionPayload, name string) bool {
	if cr.Spec.AdditionalTrustBundle.Name == name || cr.Spec.RolloutGlobalConfig.Name == name {
		return true
	}
	for _, ref := range cr.Spec.RolloutConfigMaps {
		if ref.Name == name {
			return true
		}
	}
	for _, ref := range cr.Spec.MgmtConfigMaps {
		if ref.Name == name {
			return true
		}
	}
	return false
}
