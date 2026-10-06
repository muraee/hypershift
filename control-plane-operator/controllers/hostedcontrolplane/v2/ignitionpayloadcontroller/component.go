package ignitionpayloadcontroller

import (
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"
)

const (
	// ComponentName is the name of the ignition payload controller component. It is also the
	// Deployment name, container name, and ServiceAccount/Role name.
	ComponentName = "ignition-payload-controller"
)

var _ component.ComponentOptions = &Options{}

// Options configures the ignition-payload-controller component. It runs the HO image's
// ignition-payload-controller subcommand (leader-elected payload generator) in the HCP namespace.
type Options struct {
	HyperShiftOperatorImage string
	ReleaseProvider         releaseinfo.ProviderWithOpenShiftImageRegistryOverrides
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (o *Options) IsRequestServing() bool { return false }

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (o *Options) MultiZoneSpread() bool { return true }

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (o *Options) NeedsManagementKASAccess() bool { return true }

// NewComponent returns the ignition-payload-controller ControlPlaneComponent. It is NOT yet wired
// into the HostedCluster reconcile; Phase 5c reconciles it (gated on the IgnitionPayloadSystem
// feature gate) and swaps it in for the legacy ignition-server.
func NewComponent(opts *Options) component.ControlPlaneComponent {
	return component.NewDeploymentComponent(ComponentName, opts).
		WithAdaptFunction(opts.adaptDeployment).
		Build()
}
