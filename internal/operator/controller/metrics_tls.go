package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
)

const (
	metricsTLSSecret   = "scion-node-agent-metrics-tls"
	metricsCAConfigMap = "scion-node-agent-metrics-ca"
	// metricsCertValidity balances rotation churn against exposure; the
	// operator re-issues once less than metricsCertRenewBefore remains.
	metricsCertValidity    = 365 * 24 * time.Hour
	metricsCertRenewBefore = 30 * 24 * time.Hour
)

// metricsServiceDNS is the name scrapers must set as TLS ServerName; it
// matches the metrics Service in config/manifests/monitoring.yaml.
var metricsServiceDNS = []string{
	"scion-node-agent-metrics." + render.Namespace + ".svc",
	"scion-node-agent-metrics." + render.Namespace + ".svc.cluster.local",
}

// ensureMetricsTLS provides the agent's metrics serving certificate on
// clusters without the OpenShift service-ca (which otherwise fills the same
// Secret via the annotated Service — the operator must not fight it, hence
// the openshift guard in the caller). A self-signed CA and leaf are
// generated and rotated before expiry; the CA certificate is published in a
// ConfigMap for scrapers to pin.
func (r *ScionNetworkReconciler) ensureMetricsTLS(ctx context.Context, sn *v1alpha1.ScionNetwork) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: metricsTLSSecret, Namespace: render.Namespace},
	}
	err := r.Get(ctx, types.NamespacedName{Namespace: render.Namespace, Name: metricsTLSSecret}, secret)
	if err == nil && metricsCertUsable(secret.Data["tls.crt"]) {
		return nil
	}

	certPEM, keyPEM, caPEM, genErr := generateMetricsCert()
	if genErr != nil {
		return fmt.Errorf("generate metrics serving certificate: %w", genErr)
	}
	secret.Type = corev1.SecretTypeTLS
	if err := r.applyObject(ctx, sn, secret, func() error {
		secret.Type = corev1.SecretTypeTLS
		secret.Data = map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caPEM,
		}
		return nil
	}); err != nil {
		return fmt.Errorf("apply metrics TLS secret: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: metricsCAConfigMap, Namespace: render.Namespace},
	}
	if err := r.applyObject(ctx, sn, cm, func() error {
		cm.Data = map[string]string{"ca.crt": string(caPEM)}
		return nil
	}); err != nil {
		return fmt.Errorf("apply metrics CA configmap: %w", err)
	}
	return nil
}

// metricsCertUsable reports whether the stored certificate covers the
// metrics Service DNS name and does not expire soon. It intentionally
// accepts certificates from any issuer: on OpenShift service-ca owns the
// secret contents, and rotating those would fight the platform.
func metricsCertUsable(certPEM []byte) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Now().Add(metricsCertRenewBefore).After(cert.NotAfter) {
		return false
	}
	return cert.VerifyHostname(metricsServiceDNS[0]) == nil
}

// generateMetricsCert mints a fresh single-purpose CA and a serving leaf
// for the metrics Service DNS names. The CA key is discarded: nothing else
// is ever signed with it, so a compromise of the secret only exposes this
// one identity.
func generateMetricsCert() (certPEM, keyPEM, caPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "scion-node-agent-metrics-ca"},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(metricsCertValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: metricsServiceDNS[0]},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(metricsCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     metricsServiceDNS,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return certPEM, keyPEM, caPEM, nil
}
