package ignitionpayload

import (
	"context"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utiluuid "k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	conditionPayloadGenerated = "PayloadGenerated"
	conditionIgnitionReached  = "IgnitionReached"
)

// Reconciler generates ignition payloads for IgnitionPayload CRs: it reads the config the CR
// names, validates it, computes the payload-identity and rollout hashes, renders+stores payloads,
// and advances status to drive rollouts.
type Reconciler struct {
	client.Client
	Store     payloadstore.PayloadStore
	Generator *payloadGenerator
	Namespace string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &hyperv1.IgnitionPayload{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	owner := payloadstore.OwnerRef{Namespace: cr.Namespace, Name: cr.Name}

	// Read the credential/global-config inputs the CR names. Read failures are transient
	// (the referenced object may not exist yet) and requeue.
	pullSecretContent, err := r.readSecretKey(ctx, cr.Namespace, cr.Spec.PullSecretName, corev1.DockerConfigJsonKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	trustBundleContent, err := r.readConfigMapKey(ctx, cr.Namespace, cr.Spec.AdditionalTrustBundle.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	rolloutGlobalConfig, err := r.readConfigMapKey(ctx, cr.Namespace, cr.Spec.RolloutGlobalConfig.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Validation gate: an invalid config sets PayloadGenerated=False and stops. No hash advance,
	// no token, no rollout.
	resolved, err := resolveAndValidate(ctx, r.Client, cr)
	if err != nil {
		apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
			Type:    conditionPayloadGenerated,
			Status:  metav1.ConditionFalse,
			Reason:  "InvalidConfig",
			Message: err.Error(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	in := hashInputs{
		resolved:            resolved,
		releaseVersion:      cr.Spec.ReleaseImage,
		pullSecretName:      cr.Spec.PullSecretName,
		pullSecretContent:   pullSecretContent,
		trustBundleName:     cr.Spec.AdditionalTrustBundle.Name,
		trustBundleContent:  trustBundleContent,
		rolloutGlobalConfig: rolloutGlobalConfig,
		osStream:            cr.Spec.OSStream,
	}
	identity := payloadIdentityHash(in)
	ro := rolloutHash(in)

	gen := genInputs{
		releaseImage:        cr.Spec.ReleaseImage,
		customConfig:        joinManifests(resolved),
		pullSecretHash:      supportutil.HashSimple(string(pullSecretContent)),
		trustBundleHash:     supportutil.HashSimple(string(trustBundleContent)),
		hcConfigurationHash: supportutil.HashSimple(string(rolloutGlobalConfig)),
		osStream:            cr.Spec.OSStream,
		cloudConfigHash:     "", // Phase 2: cloud-config boundary resolved with Phase 4.
	}

	// Rebaseline: if the stored formula version is older than the binary's, rewrite the stored
	// rollout hash to the new-formula value WITHOUT advancing generation (never rolls on a bump).
	if cr.Status.Current.Token != "" && cr.Status.RolloutHashVersion < rolloutHashFormulaVersion {
		cr.Status.Current.RolloutHash = ro
		cr.Status.RolloutHashVersion = rolloutHashFormulaVersion
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	// Rollout vs refresh.
	if cr.Status.Current.Token == "" || ro != cr.Status.Current.RolloutHash {
		// Rollout: move current -> previous (delete-on-evict any token previous already held),
		// mint a new token, generate+store, advance generation, reset IgnitionReached.
		if cr.Status.Previous.Token != "" {
			if err := r.Store.Delete(ctx, cr.Status.Previous.Token); err != nil {
				return ctrl.Result{}, err
			}
		}
		prevGen := cr.Status.Current.Generation
		cr.Status.Previous = cr.Status.Current

		newToken := string(utiluuid.NewUUID())
		token, _, err := r.Generator.ensurePayload(ctx, owner, identity, newToken, gen)
		if err != nil {
			return ctrl.Result{}, err
		}
		cr.Status.Current = hyperv1.PayloadReference{
			ConfigHash:  identity,
			RolloutHash: ro,
			Token:       token,
			Generation:  prevGen + 1,
		}
		cr.Status.RolloutHashVersion = rolloutHashFormulaVersion
		apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
			Type: conditionIgnitionReached, Status: metav1.ConditionFalse,
			Reason: "NewGeneration", Message: "a new payload generation has not yet been reached by a node",
		})
		apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
			Type: conditionPayloadGenerated, Status: metav1.ConditionTrue, Reason: "AsExpected",
			Message: "payload generated for the current config",
		})
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	// Refresh (Policy A): rollout hash unchanged. If the payload-identity changed (management-side
	// or credential-content change), refresh the bytes behind the current token in place.
	if identity != cr.Status.Current.ConfigHash {
		if err := r.Generator.refreshPayload(ctx, owner, cr.Status.Current.Token, identity, gen); err != nil {
			return ctrl.Result{}, err
		}
		cr.Status.Current.ConfigHash = identity
	}
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: conditionPayloadGenerated, Status: metav1.ConditionTrue, Reason: "AsExpected",
		Message: "payload generated for the current config",
	})
	return ctrl.Result{}, r.Status().Update(ctx, cr)
}

// readSecretKey returns the bytes under key in the named Secret, or nil if name is empty.
func (r *Reconciler) readSecretKey(ctx context.Context, ns, name, key string) ([]byte, error) {
	if name == "" {
		return nil, nil
	}
	s := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
		return nil, err
	}
	return s.Data[key], nil
}

// readConfigMapKey returns the "config" bytes of the named ConfigMap, or nil if name is empty.
func (r *Reconciler) readConfigMapKey(ctx context.Context, ns, name string) ([]byte, error) {
	if name == "" {
		return nil, nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm); err != nil {
		return nil, err
	}
	return []byte(cm.Data[configDataKey]), nil
}

func joinManifests(r ResolvedConfig) string {
	parts := make([]string, 0, 2)
	if r.RolloutManifests != "" {
		parts = append(parts, r.RolloutManifests)
	}
	if r.MgmtManifests != "" {
		parts = append(parts, r.MgmtManifests)
	}
	return strings.Join(parts, "\n---\n")
}
