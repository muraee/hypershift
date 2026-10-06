package ignitionpayloadserver

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"
)

const (
	// ComponentName is the name of the ignition-payload-server component. It is also the Deployment,
	// container, Service, Route, and ServiceAccount/Role name. Every resource name is distinct from
	// the legacy ignition-server so the CPOv2 framework never deletes a new resource when a
	// predicate-false legacy component reconciles.
	ComponentName = "ignition-payload-server"

	// caCertSecretName is the self-signed CA that signs both the node-facing serving cert and the
	// internal server TLS cert, and is the CA the proxy uses to verify the backend.
	caCertSecretName = ComponentName + "-ca-cert"
	// servingCertSecretName is the NODE-FACING serving cert (SAN = the Route host). The proxy
	// presents it to nodes; on IBMCloud the server presents it directly.
	servingCertSecretName = ComponentName + "-serving-cert"
	// tlsCertSecretName is the INTERNAL server cert (SANs = the Service DNS names). The server
	// presents it on 9090 behind the proxy (non-IBMCloud).
	tlsCertSecretName = ComponentName + "-tls"

	// serverProxyServiceName is the proxy Service the Route targets (non-IBMCloud). It is declared
	// here rather than imported from the proxy package to avoid an import cycle (the proxy depends on
	// this server component).
	serverProxyServiceName = ComponentName + "-proxy"
)

var _ component.ComponentOptions = &Options{}

// Options configures the ignition-payload-server component. It runs the HO image's
// ignition-payload-server subcommand (the stateless, leaderless serving tier) in the HCP namespace.
type Options struct {
	HyperShiftOperatorImage string
	DefaultIngressDomain    string

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

// NewComponent returns the ignition-payload-server ControlPlaneComponent. It owns its self-contained
// 3-secret PKI (CA + node-facing serving cert + internal server cert). It is NOT yet wired into the
// HostedCluster reconcile; Phase 5c reconciles it (gated on the IgnitionPayloadSystem feature gate).
func NewComponent(opts *Options) component.ControlPlaneComponent {
	return component.NewDeploymentComponent(ComponentName, opts).
		WithAdaptFunction(opts.adaptDeployment).
		WithPredicate(opts.predicate).
		WithManifestAdapter(
			"service.yaml",
			component.WithAdaptFunction(adaptService),
		).
		WithManifestAdapter(
			"route.yaml",
			component.WithAdaptFunction(opts.adaptRoute),
			component.WithPredicate(routePredicate),
		).
		WithManifestAdapter(
			"ca-cert.yaml",
			component.WithAdaptFunction(adaptCACertSecret),
			component.DisableIfAnnotationExist(hyperv1.DisablePKIReconciliationAnnotation),
			component.ReconcileExisting(),
		).
		WithManifestAdapter(
			"serving-cert.yaml",
			component.WithAdaptFunction(adaptServingCertSecret),
			component.DisableIfAnnotationExist(hyperv1.DisablePKIReconciliationAnnotation),
			component.ReconcileExisting(),
		).
		WithManifestAdapter(
			"tls-cert.yaml",
			component.WithAdaptFunction(adaptServerTLSSecret),
			component.DisableIfAnnotationExist(hyperv1.DisablePKIReconciliationAnnotation),
			component.ReconcileExisting(),
		).
		Build()
}

// predicate enables the component when the IgnitionPayloadSystem feature gate is on (threaded in
// via Options.Enabled) and ignition is not disabled entirely via the DisableIgnitionServerAnnotation.
// When it returns false the CPOv2 framework tears the component down, so flipping the gate off
// removes it.
func (o *Options) predicate(cpContext component.WorkloadContext) (bool, error) {
	_, disableIgnition := cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return o.Enabled && !disableIgnition, nil
}
