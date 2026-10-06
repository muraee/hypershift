package ignitionserverproxy

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitionserverv2 "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver"
	component "github.com/openshift/hypershift/support/controlplane-component"
)

const (
	ComponentName = "ignition-server-proxy"
)

var _ component.ComponentOptions = &ignitionServerProxy{}

type ignitionServerProxy struct {
	defaultIngressDomain string
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) IsRequestServing() bool {
	return true
}

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) MultiZoneSpread() bool {
	return true
}

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) NeedsManagementKASAccess() bool {
	return false
}

func NewComponent(defaultIngressDomain string) component.ControlPlaneComponent {
	ignition := &ignitionServerProxy{
		defaultIngressDomain: defaultIngressDomain,
	}

	return component.NewDeploymentComponent(ComponentName, ignition).
		WithAdaptFunction(adaptDeployment).
		WithPredicate(predicate).
		WithManifestAdapter(
			"haproxy-config.yaml",
			component.WithAdaptFunction(adaptHAProxyConfig),
		).
		WithManifestAdapter(
			"service.yaml",
			component.WithAdaptFunction(adaptService),
		).
		WithDependencies(ignitionserverv2.ComponentName).
		Build()
}

func predicate(cpContext component.WorkloadContext) (bool, error) {
	// Stand down the legacy ignition-server-proxy once the re-architected ignition-payload stack is
	// deployed and Available. The HyperShift Operator sets this annotation (gate ON + new server
	// Available), so the legacy stack is torn down only after the new one serves (no serving gap).
	if cpContext.HCP.Annotations[hyperv1.IgnitionPayloadActiveAnnotation] == "true" {
		return false, nil
	}
	_, disableIgnition := cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return !disableIgnition && cpContext.HCP.Spec.Platform.Type != hyperv1.IBMCloudPlatform, nil
}
