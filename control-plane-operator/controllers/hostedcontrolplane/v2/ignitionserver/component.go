package ignitionserver

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/releaseinfo"
)

const (
	ComponentName = "ignition-server"
)

var _ component.ComponentOptions = &ignitionServer{}

type ignitionServer struct {
	releaseProvider      releaseinfo.ProviderWithOpenShiftImageRegistryOverrides
	defaultIngressDomain string

	// newModel selects the stateless serving tier of the split ignition system (HyperShift Operator
	// image, ignition-payload-server subcommand, token-verification key) instead of the legacy
	// generation+serving monolith (control-plane-operator image, ignition-server subcommand). Both
	// share the ignition-server component name, asset directory, Service, Route and serving PKI, so the
	// node-facing endpoint and CA are unchanged across the cutover.
	newModel bool
	// hypershiftOperatorImage is the image the serving tier runs in new-model mode; empty in legacy mode.
	hypershiftOperatorImage string
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (r *ignitionServer) IsRequestServing() bool {
	return false
}

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (r *ignitionServer) MultiZoneSpread() bool {
	return true
}

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (r *ignitionServer) NeedsManagementKASAccess() bool {
	return true
}

// NewComponent builds the legacy generation+serving ignition monolith reconciled by the CPO from the
// control-plane release payload.
func NewComponent(releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides, defaultIngressDomain string) component.ControlPlaneComponent {
	return (&ignitionServer{
		releaseProvider:      releaseProvider,
		defaultIngressDomain: defaultIngressDomain,
	}).build()
}

// NewServerComponent builds the stateless serving tier of the split ignition system, reconciled by the
// HyperShift Operator from its own image. It reuses the ignition-server component name, asset directory,
// Service, Route and serving PKI, so the node-facing endpoint and CA do not change across the cutover.
func NewServerComponent(hypershiftOperatorImage, defaultIngressDomain string) component.ControlPlaneComponent {
	return (&ignitionServer{
		newModel:                true,
		hypershiftOperatorImage: hypershiftOperatorImage,
		defaultIngressDomain:    defaultIngressDomain,
	}).build()
}

func (ign *ignitionServer) build() component.ControlPlaneComponent {
	builder := component.NewDeploymentComponent(ComponentName, ign).
		WithAdaptFunction(ign.adaptDeployment).
		WithPredicate(predicate).
		WithManifestAdapter(
			"service.yaml",
			component.WithAdaptFunction(adaptService),
		).
		WithManifestAdapter(
			"podmonitor.yaml",
			component.WithAdaptFunction(adaptPodMonitor),
		).
		WithManifestAdapter(
			"route.yaml",
			component.WithAdaptFunction(ign.adaptRoute),
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
		)

	// The stateless serving tier needs only to read the PayloadStore and record IgnitionReached; it
	// drops the monolith's config/hostedcontrolplanes access. Legacy mode keeps the asset role as-is.
	if ign.newModel {
		builder = builder.WithManifestAdapter(
			"role.yaml",
			component.WithAdaptFunction(adaptServerRole),
		)
	}

	return builder.Build()
}

func predicate(cpContext component.WorkloadContext) (bool, error) {
	_, disableIgnition := cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return !disableIgnition, nil
}
