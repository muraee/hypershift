// Package ignitionpayloadcutover holds the small, shared predicates that gate the re-architected
// ignition payload stack. They live here (rather than in the hostedcluster package) so BOTH the
// hostedcluster controller and the nodepool controller can key their gate-ON behavior on the exact
// same signals without importing each other.
package ignitionpayloadcutover

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	"github.com/openshift/hypershift/hypershift-operator/featuregate"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HasDisableIgnitionServerAnnotation reports whether the operator set DisableIgnitionServerAnnotation
// on the HostedCluster, i.e. requested no ignition at all. This reads the HostedCluster (operator
// intent), which is distinct from the same annotation on the HostedControlPlane — the HO also sets
// the HCP annotation purely as the legacy-standdown cutover signal, so the HCP value cannot be used
// to infer operator intent.
func HasDisableIgnitionServerAnnotation(hcluster *hyperv1.HostedCluster) bool {
	_, ok := hcluster.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return ok
}

// Active reports whether the data plane has cut over to the re-architected ignition payload stack for
// this HostedControlPlane. It is true when the IgnitionPayloadSystem management feature gate is
// enabled AND the new ignition-payload-server ControlPlaneComponent reports Available. This drives
// both endpoint re-pointing and the legacy-standdown signal, so the legacy ignition-server is only
// disabled once the new server is actually serving (no serving gap). The nodepool controller uses it
// to decide whether to write node userdata pointing at the new endpoint.
func Active(ctx context.Context, c client.Client, hcpNamespace string) (bool, error) {
	if !featuregate.Gate().Enabled(featuregate.IgnitionPayloadSystem) {
		return false, nil
	}
	cr := &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: hcpNamespace, Name: ignitionpayloadserver.ComponentName},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get %s ControlPlaneComponent: %w", ignitionpayloadserver.ComponentName, err)
	}
	return meta.IsStatusConditionTrue(cr.Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable)), nil
}
