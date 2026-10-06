package ignitionpayload

import (
	"context"
	"errors"
	"fmt"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	"github.com/openshift/hypershift/support/releaseinfo"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utiluuid "k8s.io/apimachinery/pkg/util/uuid"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	conditionPayloadGenerated = "PayloadGenerated"
	conditionIgnitionReached  = "IgnitionReached"

	// storeCleanupFinalizer is removed only after every PayloadStore token for the CR has
	// been freed. It keeps store reclamation consumer-agnostic (the consumer owns its own
	// finalizer separately).
	storeCleanupFinalizer = "hypershift.openshift.io/ignition-payload-store-cleanup"

	// trustBundleKey is the ConfigMap data key holding the additional trust bundle, matching the
	// key GetPayload reads and hashes.
	trustBundleKey = "ca-bundle.crt"
)

// Reconciler generates ignition payloads for IgnitionPayload CRs: it reads the config the CR
// names, validates it, computes the payload-identity and rollout hashes, renders+stores payloads,
// and advances status to drive rollouts.
type Reconciler struct {
	client.Client
	Store     payloadstore.PayloadStore
	Generator *payloadGenerator
	// ReleaseProvider resolves a release-image pullspec to its OCP version for the rollout/identity
	// hashes (matching the deployed ConfigGenerator.Hash, which keys on releaseImage.Version()).
	ReleaseProvider releaseinfo.Provider
	Namespace       string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &hyperv1.IgnitionPayload{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	owner := payloadstore.OwnerRef{Namespace: cr.Namespace, Name: cr.Name}

	// Teardown: free every remaining store token for the CR, then drop the finalizer.
	if !cr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cr, storeCleanupFinalizer) {
			if err := r.freeAllTokens(ctx, owner); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(cr, storeCleanupFinalizer)
			if err := r.Update(ctx, cr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the store-cleanup finalizer is present before we write anything to the store.
	if !controllerutil.ContainsFinalizer(cr, storeCleanupFinalizer) {
		controllerutil.AddFinalizer(cr, storeCleanupFinalizer)
		if err := r.Update(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Read the credential/global-config inputs the CR names. Read failures are transient
	// (the referenced object may not exist yet) and requeue.
	pullSecretContent, err := r.readSecretKey(ctx, cr.Namespace, cr.Spec.PullSecretName, corev1.DockerConfigJsonKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	trustBundleContent, err := r.readConfigMapKey(ctx, cr.Namespace, cr.Spec.AdditionalTrustBundle.Name, trustBundleKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	rolloutGlobalConfig, err := r.readConfigMapKey(ctx, cr.Namespace, cr.Spec.RolloutGlobalConfig.Name, configDataKey)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Validation gate: a manifest that fails defaulting/validation sets PayloadGenerated=False and
	// stops (no hash advance, no token, no rollout). A transient read error (e.g. a ConfigMap that
	// is briefly absent) is NOT a validation failure and requeues instead.
	resolved, err := resolveAndValidate(ctx, r.Client, cr)
	if err != nil {
		var ve *validationError
		if errors.As(err, &ve) {
			apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
				Type:    conditionPayloadGenerated,
				Status:  metav1.ConditionFalse,
				Reason:  "InvalidConfig",
				Message: err.Error(),
			})
			return ctrl.Result{}, r.Status().Update(ctx, cr)
		}
		return ctrl.Result{}, err
	}

	// Resolve the release pullspec to its OCP version for the hashes, matching the deployed
	// ConfigGenerator.Hash() which keys on releaseImage.Version() (not the pullspec). A Lookup
	// failure is transient and requeues; it is not a config-validation failure.
	releaseImage, err := r.ReleaseProvider.Lookup(ctx, cr.Spec.ReleaseImage, pullSecretContent)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to resolve release image %q: %w", cr.Spec.ReleaseImage, err)
	}

	in := hashInputs{
		resolved:            resolved,
		releaseVersion:      releaseImage.Version(),
		pullSecretName:      cr.Spec.PullSecretName,
		pullSecretContent:   pullSecretContent,
		trustBundleName:     cr.Spec.AdditionalTrustBundle.Name,
		trustBundleContent:  trustBundleContent,
		rolloutGlobalConfig: rolloutGlobalConfig,
		osStream:            cr.Spec.OSStream,
	}
	identity := payloadIdentityHash(in)
	ro := rolloutHash(in)

	// Retirement: once the consumer signals a generation has drained, free the previous token.
	if cr.Status.Previous.Token != "" && cr.Spec.RetiredGeneration > 0 &&
		cr.Status.Previous.Generation <= cr.Spec.RetiredGeneration {
		if err := r.Store.Delete(ctx, cr.Status.Previous.Token); err != nil {
			return ctrl.Result{}, err
		}
		cr.Status.Previous = hyperv1.PayloadReference{}
	}

	gen := genInputs{
		releaseImage:        cr.Spec.ReleaseImage,
		customConfig:        joinManifests(resolved),
		pullSecretHash:      supportutil.HashSimple(pullSecretContent),
		trustBundleHash:     supportutil.HashSimple(trustBundleContent),
		hcConfigurationHash: supportutil.HashSimple(rolloutGlobalConfig),
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
		if err := r.sweepOrphans(ctx, owner, cr); err != nil {
			return ctrl.Result{}, err
		}
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
	if err := r.sweepOrphans(ctx, owner, cr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Status().Update(ctx, cr)
}

// freeAllTokens deletes every store entry owned by owner.
func (r *Reconciler) freeAllTokens(ctx context.Context, owner payloadstore.OwnerRef) error {
	toks, err := r.Store.ListByOwner(ctx, owner)
	if err != nil {
		return err
	}
	for _, t := range toks {
		if err := r.Store.Delete(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// sweepOrphans deletes any owner token that is not referenced by status.current or
// status.previous. It is level-triggered, single-active (the manager is leader-elected), and
// scoped to the one CR being reconciled, so it cannot race a concurrent generator. This reclaims
// entries an interrupted generation left unreferenced (crash between Put and the status write).
func (r *Reconciler) sweepOrphans(ctx context.Context, owner payloadstore.OwnerRef, cr *hyperv1.IgnitionPayload) error {
	keep := map[string]bool{}
	if cr.Status.Current.Token != "" {
		keep[cr.Status.Current.Token] = true
	}
	if cr.Status.Previous.Token != "" {
		keep[cr.Status.Previous.Token] = true
	}
	toks, err := r.Store.ListByOwner(ctx, owner)
	if err != nil {
		return err
	}
	for _, t := range toks {
		if !keep[t] {
			if err := r.Store.Delete(ctx, t); err != nil {
				return err
			}
		}
	}
	return nil
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

// readConfigMapKey returns the bytes under key in the named ConfigMap, or nil if name is empty.
func (r *Reconciler) readConfigMapKey(ctx context.Context, ns, name, key string) ([]byte, error) {
	if name == "" {
		return nil, nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm); err != nil {
		return nil, err
	}
	return []byte(cm.Data[key]), nil
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
