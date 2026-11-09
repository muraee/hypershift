//go:build e2ev2

/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/test/e2e/v2/internal"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ignitionServerGeneratorComponent is the ControlPlaneComponent the HO reconciles (from its own image)
// only when the re-architected ignition system is active; its presence is the signal that the new path
// is deployed for this control plane.
const ignitionServerGeneratorComponent = "ignition-server-generator"

// RegisterIgnitionPayloadSystemTests covers behavior specific to the re-architected (split) ignition
// system — the parts the general NodePool lifecycle specs do not assert. The general specs already run
// on this path by default and cover provisioning/rollout scenarios. Each spec skips when the new path
// is not active for the cluster under test (e.g. an older CPO without the capability label), per the
// v2 "skip, don't mutate" rule.
func RegisterIgnitionPayloadSystemTests(getTestCtx internal.TestContextGetter) {
	When("the re-architected ignition system (IgnitionPayloadSystem) is enabled", func() {
		It("deploys the split ignition components and generates a payload", Label(internal.InformingLabel, "nodepool-ignition-payload"), func() {
			testCtx := getTestCtx()
			ctx := testCtx.Context
			cpNamespace := testCtx.ControlPlaneNamespace

			if !ignitionPayloadSystemActive(ctx, testCtx.MgmtClient, cpNamespace) {
				Skip("IgnitionPayloadSystem is not active for this control plane (gate off or CPO lacks the capability label)")
			}

			hc, err := testCtx.GetHostedCluster()
			Expect(err).NotTo(HaveOccurred(), "failed to get HostedCluster")

			// The generator and serving tier run on every platform; the node-facing proxy is skipped on
			// IBM Cloud, where the server is exposed directly.
			components := []string{ignitionServerGeneratorComponent, "ignition-server"}
			if hc.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
				components = append(components, "ignition-server-proxy")
			}

			By("verifying the split ignition ControlPlaneComponents are Available")
			for _, name := range components {
				Eventually(func(g Gomega) {
					cc := &hyperv1.ControlPlaneComponent{}
					g.Expect(testCtx.MgmtClient.Get(ctx, crclient.ObjectKey{Namespace: cpNamespace, Name: name}, cc)).To(Succeed())
					g.Expect(meta.IsStatusConditionTrue(cc.Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable))).To(BeTrue(),
						"ControlPlaneComponent %s should be Available", name)
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			}

			By("verifying the generator has authored at least one IgnitionPayload with PayloadGenerated=True")
			Eventually(func(g Gomega) {
				list := &hyperv1.IgnitionPayloadList{}
				g.Expect(testCtx.MgmtClient.List(ctx, list, crclient.InNamespace(cpNamespace))).To(Succeed())
				g.Expect(list.Items).NotTo(BeEmpty(), "expected at least one IgnitionPayload CR in %s", cpNamespace)
				for i := range list.Items {
					cr := &list.Items[i]
					g.Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, hyperv1.IgnitionPayloadPayloadGeneratedCondition)).To(BeTrue(),
						"IgnitionPayload %s should report PayloadGenerated=True", cr.Name)
					g.Expect(cr.Status.Current.RolloutGeneration).To(BeNumerically(">=", int64(1)),
						"IgnitionPayload %s should have a current rolloutGeneration", cr.Name)
				}
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("reports a NodePool reaching ignition through the IgnitionPayload path", Label(internal.InformingLabel, "nodepool-ignition-payload"), func() {
			testCtx := getTestCtx()
			ctx := testCtx.Context
			cpNamespace := testCtx.ControlPlaneNamespace

			if !ignitionPayloadSystemActive(ctx, testCtx.MgmtClient, cpNamespace) {
				Skip("IgnitionPayloadSystem is not active for this control plane (gate off or CPO lacks the capability label)")
			}

			hc, err := testCtx.GetHostedCluster()
			Expect(err).NotTo(HaveOccurred(), "failed to get HostedCluster")

			defaultNP := getDefaultNodePool(ctx, testCtx.MgmtClient, hc)
			Expect(defaultNP).NotTo(BeNil(), "default NodePool should exist")

			By("verifying the NodePool reports ReachedIgnitionEndpoint=True (derived from the IgnitionPayload CR)")
			Eventually(func(g Gomega) {
				np := &hyperv1.NodePool{}
				g.Expect(testCtx.MgmtClient.Get(ctx, crclient.ObjectKeyFromObject(defaultNP), np)).To(Succeed())
				found := false
				for _, cond := range np.Status.Conditions {
					if cond.Type == hyperv1.NodePoolReachedIgnitionEndpoint {
						found = true
						g.Expect(string(cond.Status)).To(Equal(string(metav1.ConditionTrue)),
							"NodePool %s should report ReachedIgnitionEndpoint=True", np.Name)
						break
					}
				}
				g.Expect(found).To(BeTrue(), "NodePool %s should have the ReachedIgnitionEndpoint condition", np.Name)
			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying the backing IgnitionPayload CR reports IgnitionReached=True")
			Eventually(func(g Gomega) {
				list := &hyperv1.IgnitionPayloadList{}
				g.Expect(testCtx.MgmtClient.List(ctx, list, crclient.InNamespace(cpNamespace))).To(Succeed())
				reached := false
				for i := range list.Items {
					if meta.IsStatusConditionTrue(list.Items[i].Status.Conditions, hyperv1.IgnitionPayloadIgnitionReachedCondition) {
						reached = true
						break
					}
				}
				g.Expect(reached).To(BeTrue(), "at least one IgnitionPayload CR should report IgnitionReached=True")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		})
	})
}

// ignitionPayloadSystemActive reports whether the re-architected ignition system is deployed for this
// control plane, detected by the presence of the generator ControlPlaneComponent (the HO only reconciles
// it when the gate is on and the CPO advertises the capability label).
func ignitionPayloadSystemActive(ctx context.Context, c crclient.Client, cpNamespace string) bool {
	cc := &hyperv1.ControlPlaneComponent{}
	err := c.Get(ctx, crclient.ObjectKey{Namespace: cpNamespace, Name: ignitionServerGeneratorComponent}, cc)
	return err == nil
}
