package nodepool

import (
	"context"
	"fmt"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"

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
// generation has drained. It is level-triggered and monotonic: a gen at or below the
// current value is a no-op.
func advanceRetiredGeneration(ctx context.Context, c client.Client, cr *hyperv1.IgnitionPayload, gen int64) error {
	if gen <= cr.Spec.RetiredGeneration {
		return nil
	}
	patch := client.MergeFrom(cr.DeepCopy())
	cr.Spec.RetiredGeneration = gen
	if err := c.Patch(ctx, cr, patch); err != nil {
		return fmt.Errorf("failed to advance retiredGeneration to %d: %w", gen, err)
	}
	return nil
}

// ignitionConsumerInputs carries the resolved inputs the Phase-5 live reconcile supplies
// to the consumer: the gathered config ConfigMaps, the release-derived values, and the
// userdata rendering inputs. Release-image resolution, HAProxy generation, PKI/CA
// material, and the endpoint/proxy derivation are the live-reconcile wiring deferred to
// Phase 5; here they are inputs so the orchestration is unit-testable.
type ignitionConsumerInputs struct {
	releaseVersion string
	osStream       string
	userConfigs    []corev1.ConfigMap
	coreConfigs    []corev1.ConfigMap
	ntoConfigs     []corev1.ConfigMap
	haproxyRaw     string
	caCert         []byte
	endpoint       string
	proxy          *configv1.Proxy
	machineSetName string
}

// reconcileIgnitionPayloadConsumer composes the Phase-4 building blocks for one NodePool:
// author the rolloutGlobalConfig, ensure the CR exists (so its UID can own the projected
// ConfigMaps), project + classify the configs, write the full CR spec, and — once the
// PayloadController has published status.current — build the userdata Secret and (for
// InPlace) the legacy compat Secret. It returns the userdata Secret name the CAPI
// re-point will consume, or "" when no payload has been generated yet.
func reconcileIgnitionPayloadConsumer(ctx context.Context, c client.Client, store payloadstore.PayloadStore,
	nodePool *hyperv1.NodePool, hc *hyperv1.HostedCluster, in ignitionConsumerInputs) (string, error) {
	hcpNamespace := pkgmanifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)

	rolloutGlobalConfig, err := rolloutGlobalConfigData(hc, in.releaseVersion)
	if err != nil {
		return "", err
	}

	// Ensure the CR exists with a valid scalar spec first so it owns the projected
	// ConfigMaps (owner references require the owner's UID).
	scalarSpec := ignitionPayloadSpec(nodePool, hc, nil, nil, "", in.osStream)
	cr, err := reconcileIgnitionPayloadCR(ctx, c, hcpNamespace, nodePool, scalarSpec)
	if err != nil {
		return "", err
	}

	rolloutRefs, mgmtRefs, globalName, err := projectConfigs(ctx, c, hcpNamespace, cr,
		in.userConfigs, in.coreConfigs, in.ntoConfigs, in.haproxyRaw, rolloutGlobalConfig)
	if err != nil {
		return "", err
	}

	fullSpec := ignitionPayloadSpec(nodePool, hc, rolloutRefs, mgmtRefs, globalName, in.osStream)
	cr, err = reconcileIgnitionPayloadCR(ctx, c, hcpNamespace, nodePool, fullSpec)
	if err != nil {
		return "", err
	}

	// Until the PayloadController publishes a token there is nothing to serve to nodes.
	if cr.Status.Current.Token == "" {
		return "", nil
	}

	desired := userDataSecretForToken(nodePool, hcpNamespace, in.endpoint, in.caCert, in.proxy, cr.Status.Current)
	userDataSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, userDataSecret, func() error {
		if err := controllerutil.SetControllerReference(cr, userDataSecret, api.Scheme); err != nil {
			return err
		}
		userDataSecret.Data = desired.Data
		return nil
	}); err != nil {
		return "", fmt.Errorf("failed to reconcile userdata Secret %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if err := reconcileLegacyInPlaceSecret(ctx, c, store, hcpNamespace, in.machineSetName,
		nodePool.Spec.Management.UpgradeType, in.releaseVersion, cr.Status.Current); err != nil {
		return "", err
	}

	return desired.Name, nil
}

// finalizeIgnitionPayloadConsumer removes the consumer finalizer from the NodePool's
// IgnitionPayload CR so it (and its owned ConfigMaps/userdata Secret, via owner-reference
// garbage collection) can be reclaimed. Other controllers' finalizers are left intact. An
// absent CR is a no-op.
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
