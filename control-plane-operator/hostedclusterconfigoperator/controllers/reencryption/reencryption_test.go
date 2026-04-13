package reencryption

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = hyperv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

// mockMigrator implements the migrators.Migrator interface for testing.
type mockMigrator struct {
	results   map[schema.GroupResource]mockMigrationResult
	pruned    map[schema.GroupResource]bool
	hasSynced bool
}

type mockMigrationResult struct {
	finished bool
	result   error
	ts       time.Time
}

func newMockMigrator() *mockMigrator {
	return &mockMigrator{
		results:   make(map[schema.GroupResource]mockMigrationResult),
		pruned:    make(map[schema.GroupResource]bool),
		hasSynced: true,
	}
}

func (m *mockMigrator) EnsureMigration(gr schema.GroupResource, writeKey string) (bool, error, time.Time, error) {
	r, ok := m.results[gr]
	if !ok {
		return false, nil, time.Time{}, nil
	}
	return r.finished, r.result, r.ts, nil
}

func (m *mockMigrator) PruneMigration(gr schema.GroupResource) error {
	m.pruned[gr] = true
	return nil
}

func (m *mockMigrator) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockMigrator) HasSynced() bool {
	return m.hasSynced
}

const (
	testNamespace    = "clusters-test"
	testHCPName      = "test-hcp"
	testActiveKeyARN = "arn:aws:kms:us-east-1:123456789:key/test-key"
)

// testActiveKeyHash returns the deterministic fingerprint for testActiveKeyARN.
func testActiveKeyHash(t *testing.T) string {
	t.Helper()
	hcp := baseHCP()
	hash, err := computeActiveKeyFingerprint(context.Background(), fake.NewClientBuilder().WithScheme(testScheme()).Build(), hcp)
	if err != nil {
		t.Fatalf("failed to compute test fingerprint: %v", err)
	}
	return hash
}

func baseHCP() *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       testHCPName,
			Generation: 1,
		},
		Spec: hyperv1.HostedControlPlaneSpec{
			SecretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.AWS,
					AWS: &hyperv1.AWSKMSSpec{
						ActiveKey: hyperv1.AWSKMSKeyEntry{ARN: testActiveKeyARN},
					},
				},
			},
		},
	}
}

func encryptionSecret(migratedKeyHash string) *corev1.Secret {
	annotations := map[string]string{}
	if migratedKeyHash != "" {
		annotations[EncryptionMigratedKeyHashAnnotation] = migratedKeyHash
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "kas-secret-encryption-config",
			Namespace:   testNamespace,
			Annotations: annotations,
		},
	}
}

func kasDeployment(replicas int32, ready bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "kube-apiserver",
			Namespace:  testNamespace,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
		},
	}
	if ready {
		d.Status = appsv1.DeploymentStatus{
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
			ObservedGeneration: 1,
		}
	}
	return d
}

func TestReconcile(t *testing.T) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	activeHash := testActiveKeyHash(t)

	testCases := []struct {
		name               string
		hcp                *hyperv1.HostedControlPlane
		secret             *corev1.Secret
		deployment         *appsv1.Deployment
		migrator           *mockMigrator
		expectCondition    *metav1.ConditionStatus
		expectReason       string
		expectRequeue      bool
		expectMigratedHash string // expected value of migrated-key-hash annotation after reconcile
	}{
		{
			name: "When encryption is not configured it should remove the condition",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := baseHCP()
				hcp.Spec.SecretEncryption = nil
				// Pre-existing condition should be removed.
				meta.SetStatusCondition(&hcp.Status.Conditions, metav1.Condition{
					Type:   string(hyperv1.EtcdDataEncryptionUpToDate),
					Status: metav1.ConditionTrue,
					Reason: hyperv1.ReEncryptionCompletedReason,
				})
				return hcp
			}(),
			secret:             encryptionSecret(""),
			deployment:         kasDeployment(1, true),
			migrator:           newMockMigrator(),
			expectCondition:    nil,
			expectRequeue:      false,
			expectMigratedHash: "",
		},
		{
			name:               "When migrated hash is absent it should initialize it without re-encrypting",
			hcp:                baseHCP(),
			secret:             encryptionSecret(""),
			deployment:         kasDeployment(1, true),
			migrator:           newMockMigrator(),
			expectCondition:    nil,
			expectRequeue:      false,
			expectMigratedHash: activeHash,
		},
		{
			name:               "When migrated hash matches active hash it should not take any action",
			hcp:                baseHCP(),
			secret:             encryptionSecret(activeHash),
			deployment:         kasDeployment(1, true),
			migrator:           newMockMigrator(),
			expectCondition:    nil,
			expectRequeue:      false,
			expectMigratedHash: activeHash,
		},
		{
			name:               "When KAS deployment is not converged it should set WaitingForKASConvergence",
			hcp:                baseHCP(),
			secret:             encryptionSecret("old-hash"),
			deployment:         kasDeployment(2, false),
			migrator:           newMockMigrator(),
			expectCondition:    ptr.To(metav1.ConditionFalse),
			expectReason:       hyperv1.ReEncryptionWaitingForKASReason,
			expectRequeue:      true,
			expectMigratedHash: "old-hash",
		},
		{
			name:       "When migrations are in progress it should set ReEncryptionInProgress",
			hcp:        baseHCP(),
			secret:     encryptionSecret("old-hash"),
			deployment: kasDeployment(1, true),
			migrator: func() *mockMigrator {
				m := newMockMigrator()
				// All resources return not finished (default).
				return m
			}(),
			expectCondition:    ptr.To(metav1.ConditionFalse),
			expectReason:       hyperv1.ReEncryptionInProgressReason,
			expectRequeue:      true,
			expectMigratedHash: "old-hash",
		},
		{
			name:       "When all migrations succeed it should set ReEncryptionCompleted and update migrated hash",
			hcp:        baseHCP(),
			secret:     encryptionSecret("old-hash"),
			deployment: kasDeployment(1, true),
			migrator: func() *mockMigrator {
				m := newMockMigrator()
				for _, gr := range parseEncryptedResources() {
					m.results[gr] = mockMigrationResult{
						finished: true,
						result:   nil,
						ts:       time.Now(),
					}
				}
				return m
			}(),
			expectCondition:    ptr.To(metav1.ConditionTrue),
			expectReason:       hyperv1.ReEncryptionCompletedReason,
			expectRequeue:      false,
			expectMigratedHash: activeHash,
		},
		{
			name:       "When a migration fails it should set ReEncryptionFailed",
			hcp:        baseHCP(),
			secret:     encryptionSecret("old-hash"),
			deployment: kasDeployment(1, true),
			migrator: func() *mockMigrator {
				m := newMockMigrator()
				resources := parseEncryptedResources()
				// First resource fails, rest succeed.
				m.results[resources[0]] = mockMigrationResult{
					finished: true,
					result:   fmt.Errorf("migration failed"),
					ts:       time.Now(),
				}
				for _, gr := range resources[1:] {
					m.results[gr] = mockMigrationResult{
						finished: true,
						result:   nil,
						ts:       time.Now(),
					}
				}
				return m
			}(),
			expectCondition:    ptr.To(metav1.ConditionFalse),
			expectReason:       hyperv1.ReEncryptionFailedReason,
			expectRequeue:      true,
			expectMigratedHash: "old-hash",
		},
		{
			name:       "When a migration fails and retry interval has passed it should prune the migration",
			hcp:        baseHCP(),
			secret:     encryptionSecret("old-hash"),
			deployment: kasDeployment(1, true),
			migrator: func() *mockMigrator {
				m := newMockMigrator()
				resources := parseEncryptedResources()
				m.results[resources[0]] = mockMigrationResult{
					finished: true,
					result:   fmt.Errorf("migration failed"),
					ts:       time.Now().Add(-10 * time.Minute), // Beyond retry interval.
				}
				for _, gr := range resources[1:] {
					m.results[gr] = mockMigrationResult{
						finished: true,
						result:   nil,
						ts:       time.Now(),
					}
				}
				return m
			}(),
			expectCondition:    ptr.To(metav1.ConditionFalse),
			expectReason:       hyperv1.ReEncryptionFailedReason,
			expectRequeue:      true,
			expectMigratedHash: "old-hash",
		},
		{
			name:       "When migrator cache is not synced it should requeue",
			hcp:        baseHCP(),
			secret:     encryptionSecret("old-hash"),
			deployment: kasDeployment(1, true),
			migrator: func() *mockMigrator {
				m := newMockMigrator()
				m.hasSynced = false
				return m
			}(),
			expectCondition:    nil,
			expectRequeue:      true,
			expectMigratedHash: "old-hash",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			scheme := testScheme()
			objects := []client.Object{tc.hcp, tc.secret, tc.deployment}
			cpClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&hyperv1.HostedControlPlane{}).
				Build()

			r := &reEncryptionReconciler{
				cpClient:  cpClient,
				migrator:  tc.migrator,
				namespace: testNamespace,
				hcpName:   testHCPName,
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: testNamespace,
				Name:      testHCPName,
			}}

			result, err := r.Reconcile(context.Background(), req)
			g.Expect(err).ToNot(HaveOccurred())

			if tc.expectRequeue {
				g.Expect(result.RequeueAfter).To(BeNumerically(">", 0), "expected requeue")
			} else {
				g.Expect(result.RequeueAfter).To(BeZero(), "expected no requeue")
			}

			// Check HCP condition.
			hcp := &hyperv1.HostedControlPlane{}
			g.Expect(cpClient.Get(context.Background(), types.NamespacedName{
				Namespace: testNamespace, Name: testHCPName,
			}, hcp)).To(Succeed())

			cond := meta.FindStatusCondition(hcp.Status.Conditions, string(hyperv1.EtcdDataEncryptionUpToDate))
			if tc.expectCondition == nil {
				g.Expect(cond).To(BeNil(), "expected no EtcdDataEncryptionUpToDate condition")
			} else {
				g.Expect(cond).ToNot(BeNil(), "expected EtcdDataEncryptionUpToDate condition")
				g.Expect(cond.Status).To(Equal(*tc.expectCondition))
				g.Expect(cond.Reason).To(Equal(tc.expectReason))
			}

			// Check migrated-key-hash annotation on secret.
			secret := &corev1.Secret{}
			g.Expect(cpClient.Get(context.Background(), types.NamespacedName{
				Namespace: testNamespace, Name: "kas-secret-encryption-config",
			}, secret)).To(Succeed())

			if tc.expectMigratedHash != "" {
				g.Expect(secret.Annotations).To(HaveKeyWithValue(EncryptionMigratedKeyHashAnnotation, tc.expectMigratedHash))
			} else {
				g.Expect(secret.Annotations).ToNot(HaveKey(EncryptionMigratedKeyHashAnnotation))
			}
		})
	}
}

func TestReconcile_PruneMigration(t *testing.T) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	g := NewWithT(t)

	resources := parseEncryptedResources()
	failedResource := resources[0]

	m := newMockMigrator()
	m.results[failedResource] = mockMigrationResult{
		finished: true,
		result:   fmt.Errorf("migration failed"),
		ts:       time.Now().Add(-10 * time.Minute), // Beyond retry interval.
	}
	for _, gr := range resources[1:] {
		m.results[gr] = mockMigrationResult{
			finished: true,
			result:   nil,
			ts:       time.Now(),
		}
	}

	scheme := testScheme()
	cpClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(baseHCP(), encryptionSecret("old-hash"), kasDeployment(1, true)).
		WithStatusSubresource(&hyperv1.HostedControlPlane{}).
		Build()

	r := &reEncryptionReconciler{
		cpClient:  cpClient,
		migrator:  m,
		namespace: testNamespace,
		hcpName:   testHCPName,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testHCPName},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(m.pruned[failedResource]).To(BeTrue(), "expected failed migration to be pruned after retry interval")
}

func TestParseEncryptedResources(t *testing.T) {
	g := NewWithT(t)

	resources := parseEncryptedResources()
	g.Expect(resources).ToNot(BeEmpty())

	// Verify core resources have empty group.
	secretsGR := schema.GroupResource{Resource: "secrets"}
	g.Expect(resources).To(ContainElement(secretsGR))

	// Verify non-core resources have correct group.
	routesGR := schema.GroupResource{Group: "route.openshift.io", Resource: "routes"}
	g.Expect(resources).To(ContainElement(routesGR))
}

func TestComputeActiveKeyFingerprint(t *testing.T) {
	testCases := []struct {
		name              string
		secretEncryption  *hyperv1.SecretEncryptionSpec
		objects           []corev1.Secret
		expectFingerprint bool
		expectError       bool
	}{
		{
			name:              "When encryption is nil it should return empty fingerprint",
			secretEncryption:  nil,
			expectFingerprint: false,
		},
		{
			name: "When AWS KMS key is configured it should return a fingerprint",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.AWS,
					AWS: &hyperv1.AWSKMSSpec{
						ActiveKey: hyperv1.AWSKMSKeyEntry{ARN: "arn:aws:kms:us-east-1:123456789:key/test-key-1"},
					},
				},
			},
			expectFingerprint: true,
		},
		{
			name: "When Azure KMS key is configured it should return a fingerprint",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.AZURE,
					Azure: &hyperv1.AzureKMSSpec{
						ActiveKey: hyperv1.AzureKMSKey{
							KeyVaultName: "test-vault",
							KeyName:      "test-key",
							KeyVersion:   "v1",
						},
					},
				},
			},
			expectFingerprint: true,
		},
		{
			name: "When IBM Cloud KMS key is configured it should return a fingerprint",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.IBMCloud,
					IBMCloud: &hyperv1.IBMCloudKMSSpec{
						KeyList: []hyperv1.IBMCloudKMSKeyEntry{
							{CRKID: "test-crk-id", KeyVersion: 1},
						},
					},
				},
			},
			expectFingerprint: true,
		},
		{
			name: "When AESCBC key is configured it should return a fingerprint",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.AESCBC,
				AESCBC: &hyperv1.AESCBCSpec{
					ActiveKey: corev1.LocalObjectReference{Name: "aescbc-key"},
				},
			},
			objects: []corev1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "aescbc-key", Namespace: "test-namespace"},
					Data:       map[string][]byte{hyperv1.AESCBCKeySecretKey: []byte("test-key-data")},
				},
			},
			expectFingerprint: true,
		},
		{
			name: "When KMS spec is nil it should return empty fingerprint",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS:  nil,
			},
			expectFingerprint: false,
		},
		{
			name: "When encryption type is unsupported it should return an error",
			secretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: "UnsupportedType",
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			scheme := testScheme()
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
			for i := range tc.objects {
				clientBuilder.WithObjects(&tc.objects[i])
			}

			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Namespace: "test-namespace"},
				Spec: hyperv1.HostedControlPlaneSpec{
					SecretEncryption: tc.secretEncryption,
				},
			}

			fingerprint, err := computeActiveKeyFingerprint(context.Background(), clientBuilder.Build(), hcp)
			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())

			if tc.expectFingerprint {
				g.Expect(fingerprint).ToNot(BeEmpty())
				// Verify determinism: same input produces same output.
				fingerprint2, err := computeActiveKeyFingerprint(context.Background(), clientBuilder.Build(), hcp)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fingerprint).To(Equal(fingerprint2))
			} else {
				g.Expect(fingerprint).To(BeEmpty())
			}
		})
	}
}

func TestComputeActiveKeyFingerprint_DifferentKeysProduceDifferentHashes(t *testing.T) {
	g := NewWithT(t)

	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	hcp1 := &hyperv1.HostedControlPlane{
		Spec: hyperv1.HostedControlPlaneSpec{
			SecretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.AWS,
					AWS:      &hyperv1.AWSKMSSpec{ActiveKey: hyperv1.AWSKMSKeyEntry{ARN: "arn:key-1"}},
				},
			},
		},
	}

	hcp2 := &hyperv1.HostedControlPlane{
		Spec: hyperv1.HostedControlPlaneSpec{
			SecretEncryption: &hyperv1.SecretEncryptionSpec{
				Type: hyperv1.KMS,
				KMS: &hyperv1.KMSSpec{
					Provider: hyperv1.AWS,
					AWS:      &hyperv1.AWSKMSSpec{ActiveKey: hyperv1.AWSKMSKeyEntry{ARN: "arn:key-2"}},
				},
			},
		},
	}

	fp1, err := computeActiveKeyFingerprint(context.Background(), c, hcp1)
	g.Expect(err).ToNot(HaveOccurred())

	fp2, err := computeActiveKeyFingerprint(context.Background(), c, hcp2)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(fp1).ToNot(Equal(fp2))
}
