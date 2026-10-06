package ignitionserver

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPredicate(t *testing.T) {
	testCases := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{
			name:     "When ignition is not disabled, it should return true",
			expected: true,
		},
		{
			name:        "When DisableIgnitionServerAnnotation is set, it should return false",
			annotations: map[string]string{hyperv1.DisableIgnitionServerAnnotation: "true"},
			expected:    false,
		},
		{
			name:        "When IgnitionPayloadActiveAnnotation is true, it should stand down and return false",
			annotations: map[string]string{hyperv1.IgnitionPayloadActiveAnnotation: "true"},
			expected:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hcp", Namespace: "test-ns", Annotations: tc.annotations},
			}
			result, err := predicate(component.WorkloadContext{HCP: hcp})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(result).To(Equal(tc.expected))
		})
	}
}
