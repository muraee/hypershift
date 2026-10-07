package ignitionpayloadserver

import (
	"fmt"
	"net"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/certs"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/netutil"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// caCertSecretRef returns a reference to the self-contained CA secret in the HCP namespace.
func caCertSecretRef(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      caCertSecretName,
		},
	}
}

func caOpts(o *certs.CAOpts) {
	o.CASignerCertMapKey = corev1.TLSCertKey
	o.CASignerKeyMapKey = corev1.TLSPrivateKeyKey
}

// adaptCACertSecret reconciles the self-signed CA that signs both the node-facing serving cert and
// the internal server TLS cert. We only create it and don't rotate it for now.
func adaptCACertSecret(cpContext component.WorkloadContext, secret *corev1.Secret) error {
	if cpContext.SkipCertificateSigning {
		return nil
	}

	secret.Type = corev1.SecretTypeTLS
	return certs.ReconcileSelfSignedCA(secret, "ignition-payload-root-ca", "openshift", caOpts)
}

// adaptServingCertSecret reconciles the NODE-FACING serving cert (SAN = the Route host) signed by the
// self-contained CA. The proxy presents it to nodes; on IBMCloud the server presents it directly. It
// returns nil early until the CA exists and the Route is admitted with a host.
func adaptServingCertSecret(cpContext component.WorkloadContext, secret *corev1.Secret) error {
	if cpContext.SkipCertificateSigning {
		return nil
	}

	caCertSecret := caCertSecretRef(cpContext.HCP.Namespace)
	if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(caCertSecret), caCertSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get ignition payload ca-cert secret: %w", err)
	}

	serviceStrategy := netutil.ServicePublishingStrategyByTypeForHCP(cpContext.HCP, hyperv1.Ignition)
	if serviceStrategy == nil {
		return fmt.Errorf("ignition service strategy not specified")
	}

	var ignitionServerAddress string
	switch serviceStrategy.Type {
	case hyperv1.Route:
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cpContext.HCP.Namespace,
				Name:      ComponentName,
			},
		}
		if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(route), route); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to get ignition payload route: %w", err)
		}
		// The route must be admitted and assigned a host before we can generate certs.
		if len(route.Status.Ingress) == 0 || len(route.Status.Ingress[0].Host) == 0 {
			return nil
		}
		ignitionServerAddress = route.Status.Ingress[0].Host
	case hyperv1.NodePort:
		if serviceStrategy.NodePort == nil {
			return fmt.Errorf("nodeport metadata not specified for ignition service")
		}
		ignitionServerAddress = serviceStrategy.NodePort.Address
	default:
		return fmt.Errorf("unknown service strategy type for ignition service: %s", serviceStrategy.Type)
	}

	var dnsNames, ipAddresses []string
	if numericIP := net.ParseIP(ignitionServerAddress); numericIP == nil {
		dnsNames = []string{ignitionServerAddress}
	} else {
		ipAddresses = []string{ignitionServerAddress}
	}

	secret.Type = corev1.SecretTypeTLS
	return certs.ReconcileSignedCert(
		secret,
		caCertSecret,
		ComponentName,
		[]string{"openshift"},
		nil,
		corev1.TLSCertKey,
		corev1.TLSPrivateKeyKey,
		"",
		dnsNames,
		ipAddresses,
		caOpts,
	)
}

// adaptServerTLSSecret reconciles the INTERNAL server cert the server presents on 9090 (non-IBMCloud),
// signed by the self-contained CA with the in-cluster Service DNS SANs. It has NO Route dependency.
func adaptServerTLSSecret(cpContext component.WorkloadContext, secret *corev1.Secret) error {
	if cpContext.SkipCertificateSigning {
		return nil
	}

	caCertSecret := caCertSecretRef(cpContext.HCP.Namespace)
	if err := cpContext.Client.Get(cpContext, client.ObjectKeyFromObject(caCertSecret), caCertSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get ignition payload ca-cert secret: %w", err)
	}

	namespace := cpContext.HCP.Namespace
	dnsNames := []string{
		ComponentName,
		fmt.Sprintf("%s.%s.svc", ComponentName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", ComponentName, namespace),
	}

	secret.Type = corev1.SecretTypeTLS
	return certs.ReconcileSignedCert(
		secret,
		caCertSecret,
		ComponentName,
		[]string{"openshift"},
		nil,
		corev1.TLSCertKey,
		corev1.TLSPrivateKeyKey,
		"",
		dnsNames,
		nil,
		caOpts,
	)
}
