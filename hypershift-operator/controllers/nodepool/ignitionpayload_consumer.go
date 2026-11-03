package nodepool

import (
	"context"
	"fmt"
	"strings"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/capabilities"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	"github.com/openshift/hypershift/support/releaseinfo"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ignitionPayloadConsumerEnabled reports whether this NodePool takes the re-architected ignition path:
// the IgnitionPayloadSystem management feature gate is on (the 5b components are deployed by the HO) and
// the operator has not disabled ignition on the HostedCluster. When false, the legacy Token/CAPI path
// runs unchanged.
func ignitionPayloadConsumerEnabled(hcluster *hyperv1.HostedCluster, cpoSupportsIgnitionPayloadSystem bool) bool {
	if !featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem) {
		return false
	}
	// The generator/serving tier only runs when the HostedCluster's CPO advertises support for the split
	// system (it stands its own ignition components down). Without that, the consumer must stay on the
	// legacy path, or it would point NodePools at a generator that never publishes their userData.
	if !cpoSupportsIgnitionPayloadSystem {
		return false
	}
	_, disabled := hcluster.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return !disabled
}

// reconcileIgnitionPayloadConsumerForNodePool is the gate-ON live wiring: it gathers the resolved
// consumer inputs, drives reconcileIgnitionPayloadConsumer (CR authoring + projection + no-roll
// adoption), and sets the Token overrides CAPI uses to re-point Machines. It leaves token.Reconcile
// uncalled — the generator, not the Token, owns userdata on this path.
func (r *NodePoolReconciler) reconcileIgnitionPayloadConsumerForNodePool(ctx context.Context, token *Token, capi *CAPI,
	nodePool *hyperv1.NodePool, hcluster *hyperv1.HostedCluster, configGenerator *ConfigGenerator,
	releaseImage *releaseinfo.ReleaseImage, osStream, haproxyRaw, controlPlaneNamespace string) error {
	token.ignitionPayloadConsumerActive = true

	existing, err := capi.currentUserDataSecretName(ctx)
	if err != nil {
		return fmt.Errorf("failed to read current userdata Secret name: %w", err)
	}

	userConfigs, err := configGenerator.getUserConfigs(ctx)
	if err != nil {
		return fmt.Errorf("failed to get user configs: %w", err)
	}
	coreConfigs, err := configGenerator.getCoreConfigs(ctx)
	if err != nil {
		return fmt.Errorf("failed to get core configs: %w", err)
	}
	var ntoConfigs []corev1.ConfigMap
	if capabilities.IsNodeTuningCapabilityEnabled(hcluster.Spec.Capabilities) {
		ntoConfigs, err = getNTOGeneratedConfig(ctx, configGenerator)
		if err != nil {
			return fmt.Errorf("failed to get NTO configs: %w", err)
		}
	}
	additionalRolloutInputs, err := endpointConfigString(ctx, r.Client, hcluster)
	if err != nil {
		return fmt.Errorf("failed to compute endpoint rollout input: %w", err)
	}

	in := ignitionConsumerInputs{
		releaseVersion:             releaseImage.Version(),
		osStream:                   osStream,
		additionalRolloutInputs:    additionalRolloutInputs,
		userConfigs:                userConfigs,
		coreConfigs:                coreConfigs,
		ntoConfigs:                 ntoConfigs,
		haproxyRaw:                 haproxyRaw,
		machineSetName:             nodePool.GetName(),
		existingUserDataSecretName: existing,
	}

	store := payloadstore.NewSecretBackedStoreForClient(r.Client, controlPlaneNamespace)
	decision, err := reconcileIgnitionPayloadConsumer(ctx, r.Client, store, nodePool, hcluster, in)
	if err != nil {
		return err
	}

	token.userDataSecretNameOverride = decision.userDataSecretName
	token.inPlaceConfigVersionOverride = decision.inPlaceConfigVersion

	// On the one adoption reconcile, recreate not-yet-joined Machines that baked in stale userdata
	// before the content was refreshed in place. The cutoff is taken after the consumer copied the
	// content, so Machines created afterward (new userdata) are never candidates.
	if decision.sweepInFlight {
		if err := capi.sweepInFlightMachines(ctx, time.Now()); err != nil {
			return fmt.Errorf("failed to sweep in-flight Machines during adoption: %w", err)
		}
	}
	return nil
}

// enqueueNodePoolForIgnitionPayload maps an IgnitionPayload CR back to its owning NodePool via the
// back-reference annotation (ns/name). CRs without a well-formed annotation (foreign consumers,
// hand-created objects) are ignored.
func (r *NodePoolReconciler) enqueueNodePoolForIgnitionPayload(_ context.Context, obj client.Object) []reconcile.Request {
	val, ok := obj.GetAnnotations()[nodePoolAnnotation]
	if !ok {
		return nil
	}
	parts := strings.SplitN(val, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}}}
}

// advanceRetiredRolloutGeneration moves spec.retiredRolloutGeneration forward to gen once that
// generation has drained. It is level-triggered and monotonic. The guard is evaluated against a
// freshly-read object and the patch carries an optimistic lock, so the contract holds even when the
// caller passes a stale cr: a gen at or below the server value never regresses it, and a concurrent
// writer causes a conflict the caller can retry. The caller's cr is synced to the resulting value.
func advanceRetiredRolloutGeneration(ctx context.Context, c client.Client, cr *hyperv1.IgnitionPayload, gen int64) error {
	fresh := &hyperv1.IgnitionPayload{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
		return fmt.Errorf("failed to read IgnitionPayload before advancing retiredRolloutGeneration: %w", err)
	}
	if gen <= fresh.Spec.RetiredRolloutGeneration {
		cr.Spec.RetiredRolloutGeneration = fresh.Spec.RetiredRolloutGeneration
		return nil
	}
	patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
	fresh.Spec.RetiredRolloutGeneration = gen
	if err := c.Patch(ctx, fresh, patch); err != nil {
		return fmt.Errorf("failed to advance retiredRolloutGeneration to %d: %w", gen, err)
	}
	cr.Spec.RetiredRolloutGeneration = gen
	return nil
}

// ignitionConsumerInputs carries the resolved inputs the Phase-5 live reconcile supplies to the
// consumer: the gathered config ConfigMaps, the release-derived values, and the endpoint-identity
// rollout input. Release-image resolution, HAProxy generation, and the endpoint derivation
// (endpointConfigString) are the live-reconcile wiring deferred to Phase 5; here they are inputs so
// the orchestration is unit-testable. Node-facing userData authoring + token minting are no longer
// the consumer's job — the generator owns them — so caCert/endpoint/proxy are gone.
type ignitionConsumerInputs struct {
	releaseVersion string
	osStream       string
	// additionalRolloutInputs is the API-server endpoint identity (endpointConfigString) folded into
	// the rollout hash so an endpoint change rolls running nodes.
	additionalRolloutInputs string
	userConfigs             []corev1.ConfigMap
	coreConfigs             []corev1.ConfigMap
	ntoConfigs              []corev1.ConfigMap
	haproxyRaw              string
	machineSetName          string
	// existingUserDataSecretName is the pool's live CAPI Bootstrap.DataSecretName ("" for a
	// never-provisioned pool). Adoption keys off this live name so an already-provisioned pool keeps its
	// exact userdata name across gate enablement (no roll).
	existingUserDataSecretName string
}

// reconcileIgnitionPayloadConsumer composes the Phase-4 building blocks for one NodePool: author the
// rolloutGlobalConfig, ensure the CR exists (so its UID can own the projected ConfigMaps), project +
// classify the configs, write the full CR spec, and — once the generator has published status.current
// (with the userData Secret it authored) — apply the no-roll adoption decision and write the legacy
// in-place compat Secret for InPlace pools. It returns the adoptionDecision (the userdata name CAPI
// should point at, plus the InPlace config-version stamp and the in-flight-sweep flag), or the zero
// decision when no payload has been generated yet. The consumer never mints userdata or handles the
// token; the generator authors it and the consumer only references/copies it by name.
func reconcileIgnitionPayloadConsumer(ctx context.Context, c client.Client, store payloadstore.PayloadStore,
	nodePool *hyperv1.NodePool, hc *hyperv1.HostedCluster, in ignitionConsumerInputs) (adoptionDecision, error) {
	hcpNamespace := pkgmanifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)

	rolloutGlobalConfig, err := rolloutGlobalConfigData(hc, in.releaseVersion)
	if err != nil {
		return adoptionDecision{}, err
	}

	// Classify the configs into deterministic reference names first, so the full CR spec is authored
	// in a single write (no intermediate empty-ref spec that would churn the CR and thrash rollouts).
	rolloutRefs, mgmtRefs, globalName := classifyConfigs(nodePool.GetName(), in.userConfigs, in.coreConfigs, in.ntoConfigs)
	fullSpec := ignitionPayloadSpec(nodePool, hc, rolloutRefs, mgmtRefs, globalName, in.osStream, in.additionalRolloutInputs)
	cr, err := reconcileIgnitionPayloadCR(ctx, c, hcpNamespace, nodePool, fullSpec)
	if err != nil {
		return adoptionDecision{}, err
	}

	// Materialize the CR-owned ConfigMaps the spec references (owner refs need the CR's UID, now
	// available). A brief window where the spec references not-yet-created ConfigMaps is benign: the
	// generator treats a missing ConfigMap as a transient requeue.
	if err := materializeConfigs(ctx, c, hcpNamespace, cr, in.userConfigs, in.haproxyRaw, rolloutGlobalConfig); err != nil {
		return adoptionDecision{}, err
	}

	// Until the generator publishes status.current (with the userData Secret it authored) there is
	// nothing for the consumer to point machines at.
	if cr.Status.Current.UserDataSecret.Name == "" {
		return adoptionDecision{}, nil
	}

	// Apply the no-roll adoption decision (records the deployed-generation baseline on nodePool). When
	// it keeps the pool's live name, copy the generator's content under it so a scale-up Machine boots
	// new-model userdata against the new (JWT-only) server.
	decision := resolveAdoption(nodePool, cr.Status.Current, in.existingUserDataSecretName)
	if decision.copyContent {
		if err := copyUserDataSecret(ctx, c, hcpNamespace, cr.Status.Current.UserDataSecret.Name, decision.userDataSecretName, cr); err != nil {
			return adoptionDecision{}, err
		}
	}

	// For InPlace pools additionally write the byte-compatible legacy Secret the HCCO reads directly
	// (no HTTP fetch).
	if err := reconcileLegacyInPlaceSecret(ctx, c, store, in.machineSetName,
		nodePool.Spec.Management.UpgradeType, in.releaseVersion, cr.Status.Current, cr); err != nil {
		return adoptionDecision{}, err
	}

	return decision, nil
}

// finalizeIgnitionPayloadConsumer removes the consumer finalizer from the NodePool's IgnitionPayload
// CR so it (and its owned ConfigMaps + the generator-authored userData Secrets, via owner-reference
// garbage collection, once the generator's store-cleanup finalizer also clears) can be reclaimed.
// Other controllers' finalizers are left intact. An absent CR is a no-op.
func finalizeIgnitionPayloadConsumer(ctx context.Context, c client.Client, hcpNamespace, nodePoolName string) error {
	cr := &hyperv1.IgnitionPayload{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: hcpNamespace, Name: nodePoolName}, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !sets.New(cr.Finalizers...).Has(consumerFinalizer) {
		return nil
	}
	patch := client.MergeFrom(cr.DeepCopy())
	cr.Finalizers = sets.List(sets.New(cr.Finalizers...).Delete(consumerFinalizer))
	if err := c.Patch(ctx, cr, patch); err != nil {
		return fmt.Errorf("failed to remove consumer finalizer from IgnitionPayload %s/%s: %w", hcpNamespace, nodePoolName, err)
	}
	return nil
}
