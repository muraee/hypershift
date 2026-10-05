package ignitionpayloadcontroller

import (
	"context"
	"fmt"
	"os"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	ignserver "github.com/openshift/hypershift/ignition-server/controllers"
	hyperapi "github.com/openshift/hypershift/support/api"
	payloadstore "github.com/openshift/hypershift/support/ignitionpayload"
	"github.com/openshift/hypershift/support/imageregistry"
	"github.com/openshift/hypershift/support/releaseinfo"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/spf13/cobra"
)

const namespaceEnvVariableName = "MY_NAMESPACE"

var setupLog = ctrl.Log.WithName("setup")

// Options configures the ignition-payload-controller.
type Options struct {
	WorkDir             string
	Platform            string
	FeatureGateManifest string
	RegistryOverrides   map[string]string
}

// NewStartCommand returns the cobra command that runs the leader-elected ignition payload
// generator controller. It is registered as a subcommand of the HyperShift Operator binary.
func NewStartCommand() *cobra.Command {
	opts := Options{
		WorkDir: "/payloads",
	}

	cmd := &cobra.Command{
		Use:   "ignition-payload-controller",
		Short: "Leader-elected controller that generates ignition payloads for IgnitionPayload resources",
		Run: func(cmd *cobra.Command, args []string) {
			if err := run(ctrl.SetupSignalHandler(), opts); err != nil {
				setupLog.Error(err, "unable to start manager")
				os.Exit(1)
			}
		},
	}

	cmd.Flags().StringVar(&opts.WorkDir, "work-dir", opts.WorkDir, "Directory for extracted release-image files and rendering scratch space")
	cmd.Flags().StringVar(&opts.Platform, "platform", opts.Platform, "The cloud platform of the hosted cluster")
	cmd.Flags().StringVar(&opts.FeatureGateManifest, "feature-gate-manifest", opts.FeatureGateManifest, "Path to the rendered feature gate manifest")
	cmd.Flags().StringToStringVar(&opts.RegistryOverrides, "registry-overrides", opts.RegistryOverrides, "Registry override mappings applied when resolving release images")

	return cmd
}

func run(ctx context.Context, opts Options) error {
	namespace := os.Getenv(namespaceEnvVariableName)
	if namespace == "" {
		return fmt.Errorf("environment variable %s is empty, this is not supported", namespaceEnvVariableName)
	}

	setupLog.Info("Starting ignition-payload-controller", "namespace", namespace, "workDir", opts.WorkDir)

	restConfig := ctrl.GetConfigOrDie()
	restConfig.UserAgent = "ignition-payload-controller"

	leaseDuration := time.Second * 60
	renewDeadline := time.Second * 40
	retryPeriod := time.Second * 15

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        hyperapi.Scheme,
		LeaderElection:                true,
		LeaderElectionID:              "ignition-payload-controller-leader-elect",
		LeaderElectionResourceLock:    "leases",
		LeaderElectionNamespace:       namespace,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &leaseDuration,
		RenewDeadline:                 &renewDeadline,
		RetryPeriod:                   &retryPeriod,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{namespace: {}},
		},
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	imageFileCache, err := ignserver.NewImageFileCache(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("unable to create image file cache: %w", err)
	}

	imageMetadataProvider := &imageregistry.RegistryClientImageMetadataProvider{
		OpenShiftImageRegistryOverrides: imageregistry.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES")),
	}

	provider := &ignserver.LocalIgnitionProvider{
		ReleaseProvider: &releaseinfo.ProviderWithOpenShiftImageRegistryOverridesDecorator{
			Delegate: &releaseinfo.RegistryMirrorProviderDecorator{
				Delegate: &releaseinfo.CachedProvider{
					Inner: &releaseinfo.RegistryClientProvider{},
					Cache: map[string]*releaseinfo.ReleaseImage{},
				},
				RegistryOverrides: opts.RegistryOverrides,
			},
			OpenShiftImageRegistryOverrides: imageregistry.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES")),
		},
		Client:                mgr.GetClient(),
		Namespace:             namespace,
		CloudProvider:         hyperv1.PlatformType(opts.Platform),
		WorkDir:               opts.WorkDir,
		ImageFileCache:        imageFileCache,
		FeatureGateManifest:   opts.FeatureGateManifest,
		ImageMetadataProvider: imageMetadataProvider,
	}

	store := payloadstore.NewSecretBackedStore(mgr.GetClient(), namespace)
	r := ignitionpayload.NewReconciler(mgr.GetClient(), store, provider, namespace)
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up ignition payload controller: %w", err)
	}

	return mgr.Start(ctx)
}
