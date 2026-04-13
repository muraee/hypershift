package reencryption

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	resourcemanifests "github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/controllers/resources/manifests"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/operator"
	"github.com/openshift/hypershift/support/config"
	supportutil "github.com/openshift/hypershift/support/util"

	"github.com/openshift/library-go/pkg/operator/encryption/controllers/migrators"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	kubemigratorclient "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/clientset"
	migrationv1alpha1informer "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/informer"
)

const (
	ControllerName = "reencryption"

	// EncryptionMigratedKeyHashAnnotation stores the fingerprint of the encryption
	// key that etcd data is known to be encrypted with. Initialized on first
	// deployment (assuming data is already encrypted with the current key), and
	// updated after all StorageVersionMigrations complete following a key rotation.
	// Compared against the active key fingerprint (computed from HCP spec) to
	// decide whether re-encryption is needed.
	EncryptionMigratedKeyHashAnnotation = "hypershift.openshift.io/encryption-migrated-key-hash"

	migrationRetryInterval = 5 * time.Minute
	requeueInterval        = 30 * time.Second
)

func Setup(ctx context.Context, opts *operator.HostedClusterConfigOperatorConfig) error {
	// Create the kube-storage-version-migrator client from the guest cluster config.
	svmClient, err := kubemigratorclient.NewForConfig(opts.TargetConfig)
	if err != nil {
		return fmt.Errorf("failed to create storage version migration client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(opts.TargetConfig)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Create the informer factory for StorageVersionMigration CRs in the guest cluster.
	svmInformerFactory := migrationv1alpha1informer.NewSharedInformerFactory(svmClient, 10*time.Minute)
	svmInformer := svmInformerFactory.Migration().V1alpha1()

	migratorInstance := migrators.NewKubeStorageVersionMigrator(svmClient, svmInformer, discoveryClient)

	r := &reEncryptionReconciler{
		cpClient:  opts.CPCluster.GetClient(),
		migrator:  migratorInstance,
		namespace: opts.Namespace,
		hcpName:   opts.HCPName,
	}

	c, err := controller.New(ControllerName, opts.Manager, controller.Options{Reconciler: r})
	if err != nil {
		return fmt.Errorf("failed to construct controller: %w", err)
	}

	// Map any event to the single HCP reconcile request.
	mapToHCP := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: opts.Namespace, Name: opts.HCPName}}}
	}

	// Watch HCP on management cluster.
	if err := c.Watch(source.Kind(opts.CPCluster.GetCache(), &hyperv1.HostedControlPlane{},
		&handler.TypedEnqueueRequestForObject[*hyperv1.HostedControlPlane]{})); err != nil {
		return fmt.Errorf("failed to watch HCP: %w", err)
	}

	// Watch kas-secret-encryption-config Secret on management cluster.
	secretPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == manifests.KASSecretEncryptionConfigFile("").Name
	})
	if err := c.Watch(source.Kind[client.Object](opts.CPCluster.GetCache(), &corev1.Secret{},
		handler.EnqueueRequestsFromMapFunc(mapToHCP), secretPredicate)); err != nil {
		return fmt.Errorf("failed to watch encryption config secret: %w", err)
	}

	// Watch KAS Deployment on management cluster.
	deploymentPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == manifests.KASDeployment("").Name
	})
	if err := c.Watch(source.Kind[client.Object](opts.CPCluster.GetCache(), &appsv1.Deployment{},
		handler.EnqueueRequestsFromMapFunc(mapToHCP), deploymentPredicate)); err != nil {
		return fmt.Errorf("failed to watch KAS deployment: %w", err)
	}

	// Register an event handler on the SVM informer. This call is required
	// because AddEventHandler initializes the migrator's internal cacheSynced
	// function; without it, HasSynced() will panic. The handler itself is a
	// no-op because the controller reconciles on a requeue interval while
	// migrations are in progress.
	if _, err := migratorInstance.AddEventHandler(cache.ResourceEventHandlerFuncs{}); err != nil {
		return fmt.Errorf("failed to add SVM event handler: %w", err)
	}
	svmInformerFactory.Start(ctx.Done())

	return nil
}

type reEncryptionReconciler struct {
	cpClient  client.Client
	migrator  migrators.Migrator
	namespace string
	hcpName   string
}

func (r *reEncryptionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	hcp := resourcemanifests.HostedControlPlane(r.namespace, r.hcpName)
	if err := r.cpClient.Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get HCP %s: %w", req, err)
	}
	originalHCP := hcp.DeepCopy()

	result, err := r.reconcile(ctx, hcp)

	// Update HCP status if changed.
	if !reflect.DeepEqual(hcp.Status, originalHCP.Status) {
		log.Info("Updating HCP status with re-encryption condition")
		if updateErr := r.cpClient.Status().Update(ctx, hcp); updateErr != nil {
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to update HCP status: %w (original error: %v)", updateErr, err)
			}
			return reconcile.Result{}, fmt.Errorf("failed to update HCP status: %w", updateErr)
		}
	}

	if err != nil {
		return reconcile.Result{}, err
	}
	return result, nil
}

func (r *reEncryptionReconciler) reconcile(ctx context.Context, hcp *hyperv1.HostedControlPlane) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// If encryption is not configured, ensure the condition is absent.
	if hcp.Spec.SecretEncryption == nil {
		meta.RemoveStatusCondition(&hcp.Status.Conditions, string(hyperv1.EtcdDataEncryptionUpToDate))
		return reconcile.Result{}, nil
	}

	// Compute the active key fingerprint from the HCP spec.
	activeKeyHash, err := computeActiveKeyFingerprint(ctx, r.cpClient, hcp)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to compute active key fingerprint: %w", err)
	}
	if activeKeyHash == "" {
		// Cannot compute fingerprint (e.g. incomplete spec). Nothing to do.
		return reconcile.Result{}, nil
	}

	// Get the kas-secret-encryption-config secret.
	encryptionSecret := manifests.KASSecretEncryptionConfigFile(r.namespace)
	if err := r.cpClient.Get(ctx, client.ObjectKeyFromObject(encryptionSecret), encryptionSecret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Encryption config secret not found, waiting for it to be created")
			return reconcile.Result{RequeueAfter: requeueInterval}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get encryption config secret: %w", err)
	}

	// Compare the active key hash against the last-migrated key hash.
	migratedKeyHash := encryptionSecret.Annotations[EncryptionMigratedKeyHashAnnotation]
	if migratedKeyHash == "" {
		// No prior migration record. This is expected on first deployment or upgrade.
		// Initialize the migrated hash to the active hash without re-encrypting,
		// since the data is already encrypted with the current key.
		// Note: if this annotation was accidentally deleted after a key rotation,
		// this will skip re-encryption for that rotation. The next rotation will
		// trigger a full re-encryption.
		log.V(1).Info("Initializing migrated key hash — no prior migration record", "activeKeyHash", activeKeyHash)
		if err := r.setMigratedKeyHash(ctx, encryptionSecret, activeKeyHash); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to initialize migrated key hash: %w", err)
		}
		return reconcile.Result{}, nil
	}

	if migratedKeyHash == activeKeyHash {
		// Already up to date — nothing to do.
		return reconcile.Result{}, nil
	}

	log.Info("Re-encryption needed, checking KAS convergence",
		"activeKeyHash", activeKeyHash, "migratedKeyHash", migratedKeyHash)

	// Wait for KAS Deployment convergence.
	kasDeployment := manifests.KASDeployment(r.namespace)
	if err := r.cpClient.Get(ctx, client.ObjectKeyFromObject(kasDeployment), kasDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("KAS deployment not found, waiting for it to be created")
			return reconcile.Result{RequeueAfter: requeueInterval}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get KAS deployment: %w", err)
	}

	if !supportutil.IsDeploymentReady(ctx, kasDeployment) {
		log.Info("KAS deployment not yet converged, waiting")
		setReEncryptionCondition(hcp, metav1.ConditionFalse, hyperv1.ReEncryptionWaitingForKASReason,
			"Waiting for KAS deployment rollout to complete before starting re-encryption")
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// Ensure the migrator cache is synced.
	if !r.migrator.HasSynced() {
		log.Info("StorageVersionMigration informer cache not yet synced, waiting")
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// Run migrations for all encrypted resources.
	encryptedResources := parseEncryptedResources()
	allFinished := true
	var failedResources []string
	var inProgressResources []string
	var completedResources []string

	for _, gr := range encryptedResources {
		finished, result, ts, err := r.migrator.EnsureMigration(gr, activeKeyHash)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to ensure migration for %s: %w", gr, err)
		}

		if !finished {
			allFinished = false
			inProgressResources = append(inProgressResources, gr.String())
			continue
		}

		if result != nil {
			// Migration failed.
			allFinished = false
			failedResources = append(failedResources, fmt.Sprintf("%s: %v", gr, result))

			// Prune failed migration after retry interval for retry.
			if time.Since(ts) > migrationRetryInterval {
				log.Info("Pruning failed migration for retry", "resource", gr.String())
				if err := r.migrator.PruneMigration(gr); err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to prune migration for %s: %w", gr, err)
				}
			}
			continue
		}

		completedResources = append(completedResources, gr.String())
	}

	if len(failedResources) > 0 {
		msg := fmt.Sprintf("Re-encryption failed for: %s. Completed: %s",
			strings.Join(failedResources, "; "),
			strings.Join(completedResources, ", "))
		setReEncryptionCondition(hcp, metav1.ConditionFalse, hyperv1.ReEncryptionFailedReason, msg)
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	if !allFinished {
		msg := fmt.Sprintf("Re-encryption in progress for: %s. Completed: %s",
			strings.Join(inProgressResources, ", "),
			strings.Join(completedResources, ", "))
		setReEncryptionCondition(hcp, metav1.ConditionFalse, hyperv1.ReEncryptionInProgressReason, msg)
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// All migrations succeeded — set condition True, then update the migrated key hash.
	// Setting the condition first ensures that if the hash update fails, the next reconcile
	// will retry (all migrations are already complete) rather than leaving the condition
	// stuck in InProgress.
	log.Info("All re-encryption migrations completed successfully")
	setReEncryptionCondition(hcp, metav1.ConditionTrue, hyperv1.ReEncryptionCompletedReason,
		"All etcd data has been re-encrypted with the active encryption key")
	if err := r.setMigratedKeyHash(ctx, encryptionSecret, activeKeyHash); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update migrated key hash: %w", err)
	}
	return reconcile.Result{}, nil
}

func (r *reEncryptionReconciler) setMigratedKeyHash(ctx context.Context, secret *corev1.Secret, hash string) error {
	// Re-read the secret to get the latest version.
	if err := r.cpClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		return err
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[EncryptionMigratedKeyHashAnnotation] = hash
	return r.cpClient.Update(ctx, secret)
}

func setReEncryptionCondition(hcp *hyperv1.HostedControlPlane, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&hcp.Status.Conditions, metav1.Condition{
		Type:               string(hyperv1.EtcdDataEncryptionUpToDate),
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: hcp.Generation,
	})
}

// computeActiveKeyFingerprint computes a SHA-256 fingerprint of the active
// encryption key identity from the HCP spec. The fingerprint is deterministic
// and changes only when the active key changes.
func computeActiveKeyFingerprint(ctx context.Context, c client.Client, hcp *hyperv1.HostedControlPlane) (string, error) {
	secretEncryption := hcp.Spec.SecretEncryption
	if secretEncryption == nil {
		return "", nil
	}

	var input string
	switch secretEncryption.Type {
	case hyperv1.KMS:
		if secretEncryption.KMS == nil {
			return "", nil
		}
		switch secretEncryption.KMS.Provider {
		case hyperv1.AZURE:
			if secretEncryption.KMS.Azure == nil {
				return "", nil
			}
			key := secretEncryption.KMS.Azure.ActiveKey
			input = fmt.Sprintf("azure/%s/%s/%s", key.KeyVaultName, key.KeyName, key.KeyVersion)
		case hyperv1.AWS:
			if secretEncryption.KMS.AWS == nil {
				return "", nil
			}
			input = fmt.Sprintf("aws/%s", secretEncryption.KMS.AWS.ActiveKey.ARN)
		case hyperv1.IBMCloud:
			if secretEncryption.KMS.IBMCloud == nil || len(secretEncryption.KMS.IBMCloud.KeyList) == 0 {
				return "", nil
			}
			key := secretEncryption.KMS.IBMCloud.KeyList[0]
			input = fmt.Sprintf("ibmcloud/%s/%d", key.CRKID, key.KeyVersion)
		default:
			return "", fmt.Errorf("unsupported KMS provider: %s", secretEncryption.KMS.Provider)
		}
	case hyperv1.AESCBC:
		if secretEncryption.AESCBC == nil || len(secretEncryption.AESCBC.ActiveKey.Name) == 0 {
			return "", nil
		}
		activeKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretEncryption.AESCBC.ActiveKey.Name,
				Namespace: hcp.Namespace,
			},
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(activeKeySecret), activeKeySecret); err != nil {
			return "", fmt.Errorf("failed to get aescbc active key secret for fingerprint: %w", err)
		}
		keyData, ok := activeKeySecret.Data[hyperv1.AESCBCKeySecretKey]
		if !ok || len(keyData) == 0 {
			return "", nil
		}
		input = fmt.Sprintf("aescbc/%x", sha256.Sum256(keyData))
	default:
		return "", fmt.Errorf("unsupported encryption type: %s", secretEncryption.Type)
	}

	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", hash), nil
}

// parseEncryptedResources converts the string list from KMSEncryptedObjects()
// into schema.GroupResource values.
// Format: "resource" for core resources, "resource.group" for non-core.
func parseEncryptedResources() []schema.GroupResource {
	var result []schema.GroupResource
	for _, r := range config.KMSEncryptedObjects() {
		parts := strings.SplitN(r, ".", 2)
		gr := schema.GroupResource{Resource: parts[0]}
		if len(parts) > 1 {
			gr.Group = parts[1]
		}
		result = append(result, gr)
	}
	return result
}
