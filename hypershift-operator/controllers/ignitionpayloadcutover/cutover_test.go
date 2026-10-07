package ignitionpayloadcutover

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"
	"github.com/openshift/hypershift/support/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	. "github.com/onsi/gomega"
)

func TestHasDisableIgnitionServerAnnotation(t *testing.T) {
	g := NewWithT(t)

	g.Expect(HasDisableIgnitionServerAnnotation(&hyperv1.HostedCluster{})).To(BeFalse())
	g.Expect(HasDisableIgnitionServerAnnotation(&hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"}},
	})).To(BeTrue())
	// The presence of the key is what matters, not its value.
	g.Expect(HasDisableIgnitionServerAnnotation(&hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: ""}},
	})).To(BeTrue())
}

func serverComponent(ns string, available bool) *hyperv1.ControlPlaneComponent {
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	return &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ignitionpayloadserver.ComponentName},
		Status: hyperv1.ControlPlaneComponentStatus{
			Conditions: []metav1.Condition{
				{Type: string(hyperv1.ControlPlaneComponentAvailable), Status: status, Reason: "Test"},
			},
		},
	}
}

func TestActive(t *testing.T) {
	const hcpNs = "clusters-test"

	tests := []struct {
		name       string
		featureSet configv1.FeatureSet
		serverCR   *hyperv1.ControlPlaneComponent
		want       bool
	}{
		{
			name:       "gate OFF -> inactive (no CR read)",
			featureSet: configv1.Default,
			serverCR:   serverComponent(hcpNs, true),
			want:       false,
		},
		{
			name:       "gate ON, server CR missing -> inactive",
			featureSet: configv1.TechPreviewNoUpgrade,
			want:       false,
		},
		{
			name:       "gate ON, server not available -> inactive",
			featureSet: configv1.TechPreviewNoUpgrade,
			serverCR:   serverComponent(hcpNs, false),
			want:       false,
		},
		{
			name:       "gate ON, server available -> active",
			featureSet: configv1.TechPreviewNoUpgrade,
			serverCR:   serverComponent(hcpNs, true),
			want:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			previous := featuregate.FeatureSet()
			featuregate.ConfigureFeatureSet(string(tc.featureSet))
			t.Cleanup(func() { featuregate.ConfigureFeatureSet(string(previous)) })

			builder := fake.NewClientBuilder().WithScheme(api.Scheme)
			if tc.serverCR != nil {
				builder = builder.WithObjects(tc.serverCR)
			}
			fakeClient := builder.Build()

			active, err := Active(ctx, fakeClient, hcpNs)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(active).To(Equal(tc.want))
		})
	}
}
