package nodepool

import (
	"context"
	"fmt"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/capabilities"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	"github.com/openshift/hypershift/support/releaseinfo"

	configv1 "github.com/openshift/api/config/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// enqueueNodePoolForIgnitionPayload maps an IgnitionPayload CR back to its owning
// NodePool via the back-reference annotation (ns/name). CRs without a well-formed
// annotation (foreign consumers, hand-created objects) are ignored.
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

// advanceRetiredGeneration moves spec.retiredGeneration forward to gen once that
// generation has drained. It is level-triggered and monotonic. The guard is evaluated
// against a freshly-read object and the patch carries an optimistic lock, so the contract
// holds even when the caller passes a stale cr: a gen at or below the server value never
// regresses it, and a concurrent writer causes a conflict the caller can retry. The
// caller's cr is synced to the resulting value.
func advanceRetiredGeneration(ctx context.Context, c client.Client, cr *hyperv1.IgnitionPayload, gen int64) error {
	fresh := &hyperv1.IgnitionPayload{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
		return fmt.Errorf("failed to read IgnitionPayload before advancing retiredGeneration: %w", err)
	}
	if gen <= fresh.Spec.RetiredGeneration {
		cr.Spec.RetiredGeneration = fresh.Spec.RetiredGeneration
		return nil
	}
	patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
	fresh.Spec.RetiredGeneration = gen
	if err := c.Patch(ctx, fresh, patch); err != nil {
		return fmt.Errorf("failed to advance retiredGeneration to %d: %w", gen, err)
	}
	cr.Spec.RetiredGeneration = gen
	return nil
}

// ignitionConsumerInputs carries the resolved inputs the Phase-5 live reconcile supplies
// to the consumer: the gathered config ConfigMaps, the release-derived values, and the
// userdata rendering inputs. Release-image resolution, HAProxy generation, PKI/CA
// material, and the endpoint/proxy derivation are the live-reconcile wiring deferred to
// Phase 5; here they are inputs so the orchestration is unit-testable.
type ignitionConsumerInputs struct {
	releaseVersion  string
	osStream        string
	userConfigs     []corev1.ConfigMap
	coreConfigs     []corev1.ConfigMap
	ntoConfigs      []corev1.ConfigMap
	platformConfigs []corev1.ConfigMap
	haproxyRaw      string
	caCert          []byte
	endpoint        string
	proxy           *configv1.Proxy
	machineSetName  string
	// existingUserDataSecretName is the userdata Secret name the NodePool's Machines currently boot
	// from (the live Bootstrap.DataSecretName of the MachineDeployment/MachineSet), or "" for a
	// never-provisioned NodePool. Adoption keys off this LIVE name — not a derived hash — so that an
	// already-provisioned NodePool keeps its exact name across the gate flip (no roll), and a genuine
	// change rolls to the new rollout-hash name without flip-flopping mid-rollout. The legacy
	// ConfigGenerator.Hash() and the PayloadController rollout hash are computed by different formulas,
	// so the live name cannot be reconstructed from the NodePool annotation alone.
	existingUserDataSecretName string
}

// ignitionConsumerInputsFor gathers the resolved inputs the IgnitionPayload consumer needs for one
// NodePool from the shared ConfigGenerator and Token: the rollout/core/NTO/platform config
// ConfigMaps, the release-derived values, the HAProxy raw config, the self-contained node CA, and the
// userdata rendering inputs (endpoint + proxy). It mirrors the config gathering the legacy
// ConfigGenerator.Hash path does so the PayloadController renders the same node config.
func (r *NodePoolReconciler) ignitionConsumerInputsFor(ctx context.Context, hcluster *hyperv1.HostedCluster,
	nodePool *hyperv1.NodePool, cg *ConfigGenerator, token *Token, releaseImage *releaseinfo.ReleaseImage,
	resolvedRHELStream, haproxyRawConfig, controlPlaneNamespace string) (ignitionConsumerInputs, error) {
	userConfigs, err := cg.getUserConfigs(ctx)
	if err != nil {
		return ignitionConsumerInputs{}, fmt.Errorf("failed to get user configs: %w", err)
	}
	coreConfigs, err := cg.getCoreConfigs(ctx)
	if err != nil {
		return ignitionConsumerInputs{}, fmt.Errorf("failed to get core configs: %w", err)
	}
	var ntoConfigs []corev1.ConfigMap
	if capabilities.IsNodeTuningCapabilityEnabled(hcluster.Spec.Capabilities) {
		ntoConfigs, err = getNTOGeneratedConfig(ctx, cg)
		if err != nil {
			return ignitionConsumerInputs{}, fmt.Errorf("failed to get NTO generated configs: %w", err)
		}
	}
	platformConfigs, err := cg.getPlatformConfigs()
	if err != nil {
		return ignitionConsumerInputs{}, fmt.Errorf("failed to get platform configs: %w", err)
	}
	caCert, err := r.ignitionPayloadServerCACert(ctx, controlPlaneNamespace)
	if err != nil {
		return ignitionConsumerInputs{}, err
	}
	return ignitionConsumerInputs{
		releaseVersion:  releaseImage.Version(),
		osStream:        resolvedRHELStream,
		userConfigs:     userConfigs,
		coreConfigs:     coreConfigs,
		ntoConfigs:      ntoConfigs,
		platformConfigs: platformConfigs,
		haproxyRaw:      haproxyRawConfig,
		caCert:          caCert,
		endpoint:        hcluster.Status.IgnitionEndpoint,
		proxy:           token.userData.proxy,
		machineSetName:  nodePool.GetName(),
	}, nil
}

// ignitionPayloadServerCACert reads the self-contained CA cert the re-architected ignition stack
// signs its node-facing serving cert with, from the ignition-payload-server-ca-cert Secret in the HCP
// namespace. The CA is embedded in the node userdata so booting nodes trust the new server/proxy.
func (r *NodePoolReconciler) ignitionPayloadServerCACert(ctx context.Context, controlPlaneNamespace string) ([]byte, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: controlPlaneNamespace, Name: ignitionpayloadserver.ComponentName + "-ca-cert"},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		return nil, fmt.Errorf("failed to get ignition payload server CA cert Secret %s/%s: %w", controlPlaneNamespace, secret.Name, err)
	}
	caCert, ok := secret.Data[corev1.TLSCertKey]
	if !ok {
		return nil, fmt.Errorf("ignition payload server CA cert Secret %s/%s is missing %q", controlPlaneNamespace, secret.Name, corev1.TLSCertKey)
	}
	return caCert, nil
}

// reconcileIgnitionPayloadConsumer composes the Phase-4 building blocks for one NodePool:
// author the rolloutGlobalConfig, ensure the CR exists (so its UID can own the projected
// ConfigMaps), project + classify the configs, and write the full CR spec. The CR and its
// projected ConfigMaps are ALWAYS reconciled under the gate (so the PayloadController generates a
// payload and the new server can serve it). Only once the data plane has cut over to the new
// endpoint (cutoverActive) AND the PayloadController has published status.current does it build the
// userdata Secret (and, for InPlace, the legacy compat Secret) and return the Secret name the CAPI
// re-point consumes. Before cutover the ignition endpoint still points at the legacy server, so
// writing userdata carrying the new token/CA + legacy endpoint would break newly booting nodes;
// returning "" leaves existing Machines on their live legacy userdata. It always returns the CR so
// callers can drive adoption/retirement off its status.
func reconcileIgnitionPayloadConsumer(ctx context.Context, c client.Client, store payloadstore.PayloadStore,
	nodePool *hyperv1.NodePool, hc *hyperv1.HostedCluster, cutoverActive bool, in ignitionConsumerInputs) (string, *hyperv1.IgnitionPayload, error) {
	hcpNamespace := pkgmanifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)

	rolloutGlobalConfig, err := rolloutGlobalConfigData(hc, in.releaseVersion)
	if err != nil {
		return "", nil, err
	}

	// Classify the configs into deterministic reference names first, so the full CR spec is
	// authored in a single write (no intermediate empty-ref spec that would churn the CR).
	rolloutRefs, mgmtRefs, globalName := classifyConfigs(nodePool.GetName(), in.userConfigs, in.coreConfigs, in.ntoConfigs, in.platformConfigs)
	fullSpec := ignitionPayloadSpec(nodePool, hc, rolloutRefs, mgmtRefs, globalName, in.osStream)
	cr, err := reconcileIgnitionPayloadCR(ctx, c, hcpNamespace, nodePool, fullSpec)
	if err != nil {
		return "", nil, err
	}

	// Materialize the CR-owned ConfigMaps the spec references (owner refs need the CR's UID,
	// now available). A brief window where the spec references not-yet-created ConfigMaps is
	// benign: the PayloadController treats a missing ConfigMap as a transient requeue.
	if err := materializeConfigs(ctx, c, hcpNamespace, cr, in.userConfigs, in.platformConfigs, in.haproxyRaw, rolloutGlobalConfig); err != nil {
		return "", cr, err
	}

	// Before cutover the endpoint is still the legacy server, and until the PayloadController
	// publishes a token there is nothing to serve to nodes. In both cases we defer userdata so
	// existing nodes stay on their live legacy userdata and no half-configured userdata is written.
	if !cutoverActive || cr.Status.Current.Token == "" {
		return "", cr, nil
	}

	userDataName := adoptUserDataSecretName(nodePool, cr.Status.Current, in.existingUserDataSecretName)

	desired := userDataSecretForToken(nodePool, hcpNamespace, in.endpoint, in.caCert, in.proxy, cr.Status.Current)
	userDataSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: hcpNamespace, Name: userDataName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, userDataSecret, func() error {
		if err := controllerutil.SetControllerReference(cr, userDataSecret, api.Scheme); err != nil {
			return err
		}
		userDataSecret.Data = desired.Data
		return nil
	}); err != nil {
		return "", cr, fmt.Errorf("failed to reconcile userdata Secret %s/%s: %w", hcpNamespace, userDataName, err)
	}

	if err := reconcileLegacyInPlaceSecret(ctx, c, store, hcpNamespace, in.machineSetName,
		nodePool.Spec.Management.UpgradeType, in.releaseVersion, cr.Status.Current, cr); err != nil {
		return "", cr, err
	}

	return userDataName, cr, nil
}

// adoptUserDataSecretName decides the userdata Secret name the consumer writes content under (and
// returns for the CAPI re-point), and records the adopted rollout-hash baseline on the NodePool. It
// is the crux of the no-fleet-roll-on-gate-flip guarantee:
//
//   - Already-provisioned NodePool whose adoption marker is absent (first gate-ON reconcile) or equals
//     the current rollout hash (steady state): keep the EXISTING live userdata name so CAPI sees no
//     DataSecretName change and does not roll; the content is overwritten in place with the new
//     token/CA/endpoint, which scale-up Machines pick up.
//   - Brand-new NodePool (no live userdata): name keys on the current rollout hash.
//   - Genuine config change after adoption (current rollout hash differs from the marker): name keys
//     on the new rollout hash, so CAPI rolls normally.
//
// In all three it records current.RolloutHash as the adopted baseline so the next reconcile can tell
// steady state from a genuine change.
func adoptUserDataSecretName(nodePool *hyperv1.NodePool, current hyperv1.PayloadReference, existing string) string {
	marker := nodePool.GetAnnotations()[nodePoolAnnotationIgnitionAdoptedRolloutHash]
	rolloutHashName := fmt.Sprintf("%s-%s-%s", UserDataSecrePrefix, nodePool.GetName(), current.RolloutHash)

	var name string
	switch {
	case existing == "":
		// Brand-new NodePool: nothing to adopt, key on the rollout hash.
		name = rolloutHashName
	case marker == "" || marker == current.RolloutHash:
		// Already provisioned and not yet adopted, or steady state: keep the existing name (no roll).
		name = existing
	default:
		// Genuine change after adoption: new rollout hash -> new name -> CAPI rolls.
		name = rolloutHashName
	}

	if nodePool.Annotations == nil {
		nodePool.Annotations = map[string]string{}
	}
	nodePool.Annotations[nodePoolAnnotationIgnitionAdoptedRolloutHash] = current.RolloutHash
	return name
}

// finalizeIgnitionPayloadConsumer tears down the NodePool's IgnitionPayload CR when the NodePool is
// deleted: it deletes the CR (so the PayloadController frees the CR's store tokens via its own
// storeCleanupFinalizer, and the CR-owned projected ConfigMaps + userdata/compat Secrets are
// garbage-collected) and removes the consumer finalizer. Without the delete the CR, its store token,
// and its owned Secrets would leak until HostedCluster teardown. Both steps are idempotent; an absent
// CR is a no-op.
func finalizeIgnitionPayloadConsumer(ctx context.Context, c client.Client, hcpNamespace, nodePoolName string) error {
	cr := &hyperv1.IgnitionPayload{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: hcpNamespace, Name: nodePoolName}, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Request deletion so the PayloadController runs its store-cleanup finalizer and owner-reference GC
	// reclaims the CR-owned resources. Idempotent: no-op once a deletion timestamp is set.
	if cr.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete IgnitionPayload %s/%s: %w", hcpNamespace, nodePoolName, err)
		}
	}
	// Drop the consumer finalizer; the CR is reclaimed once the PayloadController also removes its
	// store-cleanup finalizer. Other controllers' finalizers are left intact.
	if sets.New(cr.Finalizers...).Has(consumerFinalizer) {
		patch := client.MergeFrom(cr.DeepCopy())
		cr.Finalizers = sets.List(sets.New(cr.Finalizers...).Delete(consumerFinalizer))
		if err := c.Patch(ctx, cr, patch); err != nil {
			return fmt.Errorf("failed to remove consumer finalizer from IgnitionPayload %s/%s: %w", hcpNamespace, nodePoolName, err)
		}
	}
	return nil
}
