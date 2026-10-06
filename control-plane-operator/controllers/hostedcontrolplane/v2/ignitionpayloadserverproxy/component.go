package ignitionpayloadserverproxy

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionpayloadserver"
	component "github.com/openshift/hypershift/support/controlplane-component"
)

const (
	// ComponentName is the name of the ignition-payload-server-proxy component. It is also the
	// Deployment and Service name. Every resource name is distinct from the legacy
	// ignition-server-proxy so the CPOv2 framework never deletes a new resource when a
	// predicate-false legacy component reconciles.
	ComponentName = "ignition-payload-server-proxy"
)

var _ component.ComponentOptions = &Options{}

// Options configures the ignition-payload-server-proxy component. The proxy is the request-serving
// isolation boundary: it terminates node TLS with the node-facing serving cert and forwards to the
// stateless ignition-payload-server backend, verifying it against the self-contained CA.
type Options struct {
	// Enabled gates the whole component on the HO-side IgnitionPayloadSystem feature gate.
	// It is computed by the HostedCluster reconciler (which owns the gate) and threaded in here:
	// this package lives under control-plane-operator and must NOT import
	// hypershift-operator/featuregate (that would create an import cycle).
	Enabled bool
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (o *Options) IsRequestServing() bool { return true }

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (o *Options) MultiZoneSpread() bool { return true }

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (o *Options) NeedsManagementKASAccess() bool { return false }

// NewComponent returns the ignition-payload-server-proxy ControlPlaneComponent. It depends on the
// ignition-payload-server component. It is NOT yet wired into the HostedCluster reconcile; Phase 5c
// reconciles it (gated on the IgnitionPayloadSystem feature gate).
func NewComponent(opts *Options) component.ControlPlaneComponent {
	return component.NewDeploymentComponent(ComponentName, opts).
		WithAdaptFunction(adaptDeployment).
		WithPredicate(opts.predicate).
		WithManifestAdapter(
			"haproxy-config.yaml",
			component.WithAdaptFunction(adaptHAProxyConfig),
		).
		WithManifestAdapter(
			"service.yaml",
			component.WithAdaptFunction(adaptService),
		).
		WithDependencies(ignitionpayloadserver.ComponentName).
		Build()
}

// predicate deploys the proxy when the IgnitionPayloadSystem feature gate is on (threaded in via
// Options.Enabled), on all platforms except IBMCloud (where the server Service is exposed directly),
// unless ignition is disabled entirely via the DisableIgnitionServerAnnotation. When it returns
// false the CPOv2 framework tears the component down, so flipping the gate off removes it.
func (o *Options) predicate(cpContext component.WorkloadContext) (bool, error) {
	_, disableIgnition := cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return o.Enabled && !disableIgnition && cpContext.HCP.Spec.Platform.Type != hyperv1.IBMCloudPlatform, nil
}
