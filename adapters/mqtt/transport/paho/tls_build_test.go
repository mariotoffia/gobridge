package paho

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestBuildTLSConfig_PEMPrefersOverFile verifies the dispatch rule in
// BuildTLSConfig: when both *PEM and *File are set for the same field,
// the PEM material wins. Credential rotation relies on this — a stale
// cert_file on disk must never shadow a freshly rotated PEM.
func TestBuildTLSConfig_PEMPrefersOverFile(t *testing.T) {
	caPEM := mustGenerateSelfSignedPEM(t)
	cfg := &TLSConfig{
		Enable:     true,
		CACertFile: "/does/not/exist.pem", // Would error if file path was used.
		CACertPEM:  shared.NewSecret(caPEM),
	}

	tlsCfg, err := BuildTLSConfig(cfg)
	require.NoError(t, err, "PEM material must bypass missing CACertFile")
	require.NotNil(t, tlsCfg.RootCAs)
}

// TestBuildTLSConfig_PartialPEMClientRejected pins the "both or
// neither" rule for client cert/key PEMs. A half-populated pair
// almost always indicates a bug in the credential source; failing
// loudly is safer than silently dropping to file-based material.
func TestBuildTLSConfig_PartialPEMClientRejected(t *testing.T) {
	cfg := &TLSConfig{Enable: true, CertPEM: shared.NewSecret("only cert, no key")}
	_, err := BuildTLSConfig(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CertPEM and KeyPEM")
}

// TestBuildTLSConfig_ValidPEMClientPair verifies that a real EC
// cert/key pair in PEM form parses into tls.Certificates correctly.
// Use ECDSA to keep the generated material tiny.
func TestBuildTLSConfig_ValidPEMClientPair(t *testing.T) {
	certPEM, keyPEM := mustGenerateClientPEMPair(t)
	cfg := &TLSConfig{Enable: true, CertPEM: shared.NewSecret(certPEM), KeyPEM: shared.NewSecret(keyPEM)}

	tlsCfg, err := BuildTLSConfig(cfg)
	require.NoError(t, err)
	require.Len(t, tlsCfg.Certificates, 1)
}

// TestBuildTLSConfig_EmptyTLSConfigOK covers the degenerate case:
// TLSConfig with nothing set returns an empty *tls.Config, which the
// caller uses to opt into system-default trust.
func TestBuildTLSConfig_EmptyTLSConfigOK(t *testing.T) {
	cfg := &TLSConfig{Enable: true}
	tlsCfg, err := BuildTLSConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, tlsCfg)
	require.Nil(t, tlsCfg.RootCAs)
	require.Empty(t, tlsCfg.Certificates)
}

// mustGenerateSelfSignedPEM builds a self-signed ECDSA CA suitable
// for TLS parsing. Valid-from/valid-until span 24h, more than any
// unit-test run.
func mustGenerateSelfSignedPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gobridge-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// mustGenerateClientPEMPair returns a matching cert+key in PEM form.
func mustGenerateClientPEMPair(t *testing.T) (string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "gobridge-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}
