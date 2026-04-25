package servicebus

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
)

func generateSelfSignedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestBuildTLSConfig_Nil validates that buildTLSConfig returns nil when
// neither CA PEM nor InsecureSkipVerify is set.
func TestBuildTLSConfig_Nil(t *testing.T) {
	tc, err := buildTLSConfig("", "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc != nil {
		t.Fatal("expected nil tls.Config when no CA PEM and no insecure flag")
	}
}

// TestBuildTLSConfig_InsecureSkipVerify validates that the InsecureSkipVerify
// flag is propagated to the resulting tls.Config.
func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	tc, err := buildTLSConfig("", "", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected non-nil tls.Config for insecure skip verify")
	}
	if !tc.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be true")
	}
}

// TestBuildTLSConfig_WithCACert validates that a CA PEM is loaded into the
// RootCAs pool of the resulting tls.Config.
func TestBuildTLSConfig_WithCACert(t *testing.T) {
	caPEM := generateSelfSignedCAPEM(t)

	tc, err := buildTLSConfig(caPEM, "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected non-nil tls.Config for CA PEM")
	}
	if tc.RootCAs == nil {
		t.Fatal("RootCAs should be set when CA PEM is provided")
	}
	if tc.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be false")
	}
}

// TestBuildTLSConfig_InvalidPEM validates that invalid CA PEM returns an error.
func TestBuildTLSConfig_InvalidPEM(t *testing.T) {
	_, err := buildTLSConfig("not-valid-pem", "", "", false)
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

// TestBuildClientOptions_NoTLS validates that buildClientOptions returns nil
// when no TLS configuration is needed.
func TestBuildClientOptions_NoTLS(t *testing.T) {
	cfg := ConnectionConfig{}
	opts, err := buildClientOptions(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts != nil {
		t.Fatal("expected nil options when no TLS config is set")
	}
}

// TestBuildClientOptions_WithTLSConfig validates that an explicit TLSConfig
// on ConnectionConfig is used in the returned client options.
func TestBuildClientOptions_WithTLSConfig(t *testing.T) {
	tc, _ := buildTLSConfig("", "", "", true)
	cfg := ConnectionConfig{TLSConfig: tc}
	opts, err := buildClientOptions(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts == nil {
		t.Fatal("expected non-nil options when TLSConfig is explicitly set")
	}
	if opts.TLSConfig != tc {
		t.Fatal("TLSConfig should be the provided instance")
	}
}

// TestBuildClientOptions_InvalidPEM validates that buildClientOptions returns
// an error when ConnectionConfig has invalid CaPEM.
func TestBuildClientOptions_InvalidPEM(t *testing.T) {
	cfg := ConnectionConfig{CaPEM: "garbage"}
	_, err := buildClientOptions(cfg)
	if err == nil {
		t.Fatal("expected error for invalid CaPEM in ConnectionConfig")
	}
}
