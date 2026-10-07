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

	// Enabled gates the whole component on the HO-side IgnitionPayloadSystem feature gate.
	// It is computed by the HostedCluster reconciler (which owns the gate) and threaded in here:
	// this package lives under control-plane-operator and must NOT import
	// hypershift-operator/featuregate (that would create an import cycle).
	Enabled bool
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
		WithPredicate(opts.predicate).
		WithManifestAdapter(
			"podmonitor.yaml",
			component.WithAdaptFunction(adaptPodMonitor),
		).
		Build()
}

// predicate enables the component solely on Options.Enabled, which the HyperShift Operator computes as
// "IgnitionPayloadSystem gate on AND the operator did not set DisableIgnitionServerAnnotation on the
// HostedCluster". When it returns false the CPOv2 framework tears the component down.
//
// It deliberately does NOT read DisableIgnitionServerAnnotation from the HCP: that annotation also
// carries the HO's legacy-standdown cutover signal, which must not disable the new components. Operator
// intent ("no ignition at all") is already folded into Enabled, so a per-HostedCluster operator-disable
// tears these down too.
func (o *Options) predicate(_ component.WorkloadContext) (bool, error) {
	return o.Enabled, nil
}
