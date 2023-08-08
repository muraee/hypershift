//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	hyperv1 "github.com/openshift/hypershift/api/v1beta1"
	"github.com/openshift/hypershift/cmd/cluster/core"
	"github.com/openshift/hypershift/support/conditions"
	"github.com/openshift/hypershift/support/util"
	support "github.com/openshift/hypershift/support/util"
	e2eutil "github.com/openshift/hypershift/test/e2e/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestCreateCluster implements a test that creates a cluster with the code under test
// vs upgrading to the code under test as TestUpgradeControlPlane does.
func TestCreateCluster(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(testContext)
	defer cancel()

	clusterOpts := globalOpts.DefaultClusterOptions(t)
	zones := strings.Split(globalOpts.configurableClusterOptions.Zone.String(), ",")
	if len(zones) >= 3 {
		// CreateCluster also tests multi-zone workers work properly if a sufficient number of zones are configured
		t.Logf("Sufficient zones available for InfrastructureAvailabilityPolicy HighlyAvailable")
		clusterOpts.AWSPlatform.Zones = zones
		clusterOpts.InfrastructureAvailabilityPolicy = string(hyperv1.HighlyAvailable)
		clusterOpts.NodePoolReplicas = 1
	}

	if globalOpts.RequestServingIsolation {
		clusterOpts.ControlPlaneAvailabilityPolicy = string(hyperv1.HighlyAvailable)
		clusterOpts.Annotations = append(clusterOpts.Annotations, fmt.Sprintf("%s=%s", hyperv1.TopologyAnnotation, hyperv1.DedicatedRequestServingComponentsTopology))
	}

	e2eutil.NewHypershiftTest(t, ctx, func(t *testing.T, mgtClient crclient.Client, hostedCluster *hyperv1.HostedCluster) {
		t.Logf("All good, nothing to do!")
	}).CreateCluster(&clusterOpts, globalOpts.Platform, globalOpts.ArtifactDir, globalOpts.ServiceAccountSigningKey)
}

func TestCreateClusterCustomConfig(t *testing.T) {
	if globalOpts.Platform != hyperv1.AWSPlatform {
		t.Skip("test only supported on platform AWS")
	}
	t.Parallel()
	g := NewWithT(t)

	ctx, cancel := context.WithCancel(testContext)
	defer cancel()

	client, err := e2eutil.GetClient()
	g.Expect(err).NotTo(HaveOccurred(), "failed to get k8s client")

	clusterOpts := globalOpts.DefaultClusterOptions(t)

	// find kms key ARN using alias
	kmsKeyArn, err := e2eutil.GetKMSKeyArn(clusterOpts.AWSPlatform.AWSCredentialsFile, clusterOpts.AWSPlatform.Region, globalOpts.configurableClusterOptions.AWSKmsKeyAlias)
	g.Expect(err).NotTo(HaveOccurred(), "failed to retrieve kms key arn")
	g.Expect(kmsKeyArn).NotTo(BeNil(), "failed to retrieve kms key arn")

	clusterOpts.AWSPlatform.EtcdKMSKeyARN = *kmsKeyArn

	hostedCluster := e2eutil.CreateCluster(t, ctx, client, &clusterOpts, globalOpts.Platform, globalOpts.ArtifactDir, globalOpts.ServiceAccountSigningKey)

	g.Expect(hostedCluster.Spec.SecretEncryption.KMS.AWS.ActiveKey.ARN).To(Equal(*kmsKeyArn))
	g.Expect(hostedCluster.Spec.SecretEncryption.KMS.AWS.Auth.AWSKMSRoleARN).ToNot(BeEmpty())

	validatePublicCluster(t, ctx, client, hostedCluster, &clusterOpts)

	guestClient := e2eutil.WaitForGuestClient(t, testContext, client, hostedCluster)
	e2eutil.EnsureSecretEncryptedUsingKMS(t, ctx, hostedCluster, guestClient)
	// test oauth with identity provider
	e2eutil.EnsureOAuthWithIdentityProvider(t, ctx, client, hostedCluster)
}

func TestNoneCreateCluster(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	ctx, cancel := context.WithCancel(testContext)
	defer cancel()

	client, err := e2eutil.GetClient()
	g.Expect(err).NotTo(HaveOccurred(), "failed to get k8s client")

	clusterOpts := globalOpts.DefaultClusterOptions(t)
	hostedCluster := e2eutil.CreateCluster(t, ctx, client, &clusterOpts, hyperv1.NonePlatform, globalOpts.ArtifactDir, globalOpts.ServiceAccountSigningKey)

	// Wait for the rollout to be reported complete
	t.Logf("Waiting for cluster rollout. Image: %s", globalOpts.LatestReleaseImage)
	// Since the None platform has no workers, CVO will not have expectations set,
	// which in turn means that the ClusterVersion object will never be populated.
	// Therefore only test if the control plane comes up (etc, apiserver, ...)
	e2eutil.WaitForConditionsOnHostedControlPlane(t, ctx, client, hostedCluster, globalOpts.LatestReleaseImage)

	// etcd restarts for me once always and apiserver two times before running stable
	// e2eutil.EnsureNoCrashingPods(t, ctx, client, hostedCluster)
}

// TestCreateClusterProxy implements a test that creates a cluster behind a proxy with the code under test.
func TestCreateClusterProxy(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	ctx, cancel := context.WithCancel(testContext)
	defer cancel()

	client, err := e2eutil.GetClient()
	g.Expect(err).NotTo(HaveOccurred(), "failed to get k8s client")

	clusterOpts := globalOpts.DefaultClusterOptions(t)
	clusterOpts.AWSPlatform.EnableProxy = true
	clusterOpts.ControlPlaneAvailabilityPolicy = string(hyperv1.SingleReplica)

	hostedCluster := e2eutil.CreateCluster(t, ctx, client, &clusterOpts, globalOpts.Platform, globalOpts.ArtifactDir, globalOpts.ServiceAccountSigningKey)

	validatePublicCluster(t, ctx, client, hostedCluster, &clusterOpts)
}

// TestCreateClusterPrivate implements a smoke test that creates a private cluster.
// Validations requiring guest cluster client are dropped here since the kas is not accessible when private.
// In the future we might want to leverage https://issues.redhat.com/browse/HOSTEDCP-697 to access guest cluster.
func TestCreateClusterPrivate(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	ctx, cancel := context.WithCancel(testContext)
	defer cancel()

	client, err := e2eutil.GetClient()
	g.Expect(err).NotTo(HaveOccurred(), "failed to get k8s client")

	clusterOpts := globalOpts.DefaultClusterOptions(t)
	clusterOpts.ControlPlaneAvailabilityPolicy = string(hyperv1.SingleReplica)
	clusterOpts.AWSPlatform.EndpointAccess = string(hyperv1.Private)

	hostedCluster := e2eutil.CreateCluster(t, ctx, client, &clusterOpts, globalOpts.Platform, globalOpts.ArtifactDir, globalOpts.ServiceAccountSigningKey)

	validatePrivateCluster(t, ctx, client, hostedCluster)

	// Private -> publicAndPrivate
	t.Run("SwitchFromPrivateToPublic", testSwitchFromPrivateToPublic(ctx, client, hostedCluster, &clusterOpts))
	// publicAndPrivate -> Private
	t.Run("SwitchFromPublicToPrivate", testSwitchFromPublicToPrivate(ctx, client, hostedCluster))
}

func testSwitchFromPrivateToPublic(ctx context.Context, client crclient.Client, hostedCluster *hyperv1.HostedCluster, clusterOpts *core.CreateOptions) func(t *testing.T) {
	return func(t *testing.T) {
		g := NewWithT(t)

		err := e2eutil.UpdateObject(t, ctx, client, hostedCluster, func(obj *hyperv1.HostedCluster) {
			obj.Spec.Platform.AWS.EndpointAccess = hyperv1.PublicAndPrivate
		})
		g.Expect(err).ToNot(HaveOccurred(), "failed to update hostedcluster EndpointAccess")

		validatePublicCluster(t, ctx, client, hostedCluster, clusterOpts)
	}
}

func testSwitchFromPublicToPrivate(ctx context.Context, client crclient.Client, hostedCluster *hyperv1.HostedCluster) func(t *testing.T) {
	return func(t *testing.T) {
		g := NewWithT(t)
		err := e2eutil.UpdateObject(t, ctx, client, hostedCluster, func(obj *hyperv1.HostedCluster) {
			obj.Spec.Platform.AWS.EndpointAccess = hyperv1.Private
		})
		g.Expect(err).ToNot(HaveOccurred(), "failed to update hostedcluster EndpointAccess")

		validatePrivateCluster(t, ctx, client, hostedCluster)
	}
}

func validatePublicCluster(t *testing.T, ctx context.Context, client crclient.Client, hostedCluster *hyperv1.HostedCluster, clusterOpts *core.CreateOptions) {
	g := NewWithT(t)

	// Sanity check the cluster by waiting for the nodes to report ready
	t.Logf("Waiting for guest client to become available")
	guestClient := e2eutil.WaitForGuestClient(t, testContext, client, hostedCluster)

	// Wait for Nodes to be Ready
	numNodes := clusterOpts.NodePoolReplicas * int32(len(clusterOpts.AWSPlatform.Zones))
	e2eutil.WaitForNReadyNodes(t, testContext, guestClient, numNodes, hostedCluster.Spec.Platform.Type)

	// Wait for the rollout to be complete
	t.Logf("Waiting for cluster rollout. Image: %s", globalOpts.LatestReleaseImage)
	e2eutil.WaitForImageRollout(t, testContext, client, hostedCluster, globalOpts.LatestReleaseImage)

	err := client.Get(testContext, crclient.ObjectKeyFromObject(hostedCluster), hostedCluster)
	g.Expect(err).NotTo(HaveOccurred(), "failed to get hostedcluster")

	serviceStrategy := util.ServicePublishingStrategyByTypeByHC(hostedCluster, hyperv1.APIServer)
	g.Expect(serviceStrategy).ToNot(BeNil())
	if serviceStrategy.Type == hyperv1.Route && serviceStrategy.Route != nil && serviceStrategy.Route.Hostname != "" {
		g.Expect(hostedCluster.Status.ControlPlaneEndpoint.Host).To(Equal(serviceStrategy.Route.Hostname))
	} else {
		// sanity check
		g.Expect(hostedCluster.Status.ControlPlaneEndpoint.Host).ToNot(ContainSubstring("hypershift.local"))
	}

	validateHostedClusterConditions(t, ctx, client, hostedCluster)

	e2eutil.EnsureNodeCountMatchesNodePoolReplicas(t, ctx, client, guestClient, hostedCluster.Namespace)
	e2eutil.EnsureNoCrashingPods(t, ctx, client, hostedCluster)
	e2eutil.EnsureNodeCommunication(t, ctx, client, hostedCluster)
	e2eutil.EnsurePSANotPrivileged(t, ctx, guestClient)
	e2eutil.EnsureGuestWebhooksValidated(t, ctx, guestClient)
}

func validatePrivateCluster(t *testing.T, ctx context.Context, client crclient.Client, hostedCluster *hyperv1.HostedCluster) {
	g := NewWithT(t)

	_, err := e2eutil.WaitForGuestKubeConfig(t, ctx, client, hostedCluster)
	g.Expect(err).NotTo(HaveOccurred(), "couldn't get kubeconfig")

	// Ensure NodePools have all Nodes ready.
	e2eutil.WaitForNodePoolDesiredNodes(t, ctx, client, hostedCluster)

	// Wait for the rollout to be complete
	t.Logf("Waiting for cluster rollout. Image: %s", globalOpts.LatestReleaseImage)
	e2eutil.WaitForImageRollout(t, testContext, client, hostedCluster, globalOpts.LatestReleaseImage)

	err = client.Get(testContext, crclient.ObjectKeyFromObject(hostedCluster), hostedCluster)
	g.Expect(err).NotTo(HaveOccurred(), "failed to get hostedcluster")

	serviceStrategy := util.ServicePublishingStrategyByTypeByHC(hostedCluster, hyperv1.APIServer)
	g.Expect(serviceStrategy).ToNot(BeNil())
	// Private clusters should always use Route
	g.Expect(serviceStrategy.Type).To(Equal(hyperv1.Route))
	if serviceStrategy.Route != nil && serviceStrategy.Route.Hostname != "" {
		g.Expect(hostedCluster.Status.ControlPlaneEndpoint.Host).To(Equal(serviceStrategy.Route.Hostname))
	} else {
		// sanity check
		g.Expect(hostedCluster.Status.ControlPlaneEndpoint.Host).ToNot(ContainSubstring("hypershift.local"))
	}

	validateHostedClusterConditions(t, ctx, client, hostedCluster)

	e2eutil.EnsureNoCrashingPods(t, ctx, client, hostedCluster)
}

func validateHostedClusterConditions(t *testing.T, ctx context.Context, client crclient.Client, hostedCluster *hyperv1.HostedCluster) {
	expectedConditions := conditions.ExpectedHCConditions()

	if hostedCluster.Spec.SecretEncryption == nil || hostedCluster.Spec.SecretEncryption.KMS == nil || hostedCluster.Spec.SecretEncryption.KMS.AWS == nil {
		// AWS KMS is not configured
		expectedConditions[hyperv1.ValidAWSKMSConfig] = metav1.ConditionUnknown
	} else {
		expectedConditions[hyperv1.ValidAWSKMSConfig] = metav1.ConditionTrue
	}

	kasExternalHostname := support.ServiceExternalDNSHostnameByHC(hostedCluster, hyperv1.APIServer)
	if kasExternalHostname == "" {
		// ExternalDNS is not configured
		expectedConditions[hyperv1.ExternalDNSReachable] = metav1.ConditionUnknown
	} else {
		expectedConditions[hyperv1.ExternalDNSReachable] = metav1.ConditionTrue
	}

	start := time.Now()
	err := wait.PollImmediateWithContext(ctx, 10*time.Second, 10*time.Minute, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, crclient.ObjectKeyFromObject(hostedCluster), hostedCluster); err != nil {
			t.Logf("Failed to get hostedcluster: %v", err)
			return false, nil
		}

		for _, condition := range hostedCluster.Status.Conditions {
			if condition.Type == string(hyperv1.ClusterVersionUpgradeable) {
				// ClusterVersionUpgradeable condition status is not always guranteed to be true, skip.
				t.Logf("observed condition %s status [%s]", condition.Type, condition.Status)
				continue
			}

			expectedStatus, known := expectedConditions[hyperv1.ConditionType(condition.Type)]
			if !known {
				return false, fmt.Errorf("unknown condition %s", condition.Type)
			}

			if condition.Status != expectedStatus {
				t.Logf("condition %s status [%s] doesn't match the expected status [%s]", condition.Type, condition.Status, expectedStatus)
				return false, nil
			}
			t.Logf("observed condition %s status to match expected stauts [%s]", condition.Type, expectedStatus)
		}

		return true, nil
	})
	duration := time.Since(start).Round(time.Second)

	if err != nil {
		t.Fatalf("Failed to validate HostedCluster conditions in %s: %v", duration, err)
	}
	t.Logf("Successfully validated all expected HostedCluster conditions in %s", duration)
}
