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

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/mariotoffia/gobridge/domain/shared"
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
	cfg := ConnectionConfig{CaPEM: shared.NewSecret("garbage")}
	_, err := buildClientOptions(cfg)
	if err == nil {
		t.Fatal("expected error for invalid CaPEM in ConnectionConfig")
	}
}

// --- F1: user-assigned managed identity ------------------------------------

// TestManagedIdentityCredentialOptions_SystemAssigned validates that with
// no ClientID the helper returns nil, i.e. NewManagedIdentityCredential
// accepts the default (system-assigned) identity — unchanged behaviour.
func TestManagedIdentityCredentialOptions_SystemAssigned(t *testing.T) {
	if opts := managedIdentityCredentialOptions(ConnectionConfig{UseManagedIdentity: true}); opts != nil {
		t.Fatalf("expected nil options for system-assigned identity, got %+v", opts)
	}
}

// TestManagedIdentityCredentialOptions_UserAssigned is the regression
// guard for F1: a configured ClientID MUST reach the credential options
// as a user-assigned client ID. The pre-fix code passed nil and silently
// dropped ClientID.
func TestManagedIdentityCredentialOptions_UserAssigned(t *testing.T) {
	const clientID = "11111111-2222-3333-4444-555555555555"
	opts := managedIdentityCredentialOptions(ConnectionConfig{
		UseManagedIdentity: true,
		ClientID:           clientID,
	})
	if opts == nil {
		t.Fatal("expected non-nil options when ClientID is set")
	}
	id, ok := opts.ID.(azidentity.ClientID)
	if !ok {
		t.Fatalf("expected ID of kind azidentity.ClientID, got %T", opts.ID)
	}
	if string(id) != clientID {
		t.Fatalf("ClientID = %q, want %q", string(id), clientID)
	}
}

// --- F5: configurable SDK retry policy -------------------------------------

// TestRetryConfig_ToSDK_ZeroValueKeepsDefaults asserts an unset
// RetryConfig reports hasRetry=false so buildClientOptions leaves
// RetryOptions untouched — the SDK keeps applying its own defaults (no
// behaviour change).
func TestRetryConfig_ToSDK_ZeroValueKeepsDefaults(t *testing.T) {
	if _, has := (RetryConfig{}).toSDK(); has {
		t.Fatal("zero-value RetryConfig must report hasRetry=false")
	}
}

// TestRetryConfig_ToSDK_Translates asserts each domain field maps to the
// matching SDK RetryOptions field.
func TestRetryConfig_ToSDK_Translates(t *testing.T) {
	rc := RetryConfig{MaxRetries: -1, RetryDelay: 2 * time.Second, MaxRetryDelay: 30 * time.Second}
	got, has := rc.toSDK()
	if !has {
		t.Fatal("non-zero RetryConfig must report hasRetry=true")
	}
	if got.MaxRetries != -1 {
		t.Fatalf("MaxRetries = %d, want -1", got.MaxRetries)
	}
	if got.RetryDelay != 2*time.Second {
		t.Fatalf("RetryDelay = %s, want 2s", got.RetryDelay)
	}
	if got.MaxRetryDelay != 30*time.Second {
		t.Fatalf("MaxRetryDelay = %s, want 30s", got.MaxRetryDelay)
	}
}

// TestBuildClientOptions_WithRetry asserts a configured RetryConfig
// surfaces on the returned ClientOptions even when no TLS is set (the
// pre-F5 code returned nil, dropping any retry override).
func TestBuildClientOptions_WithRetry(t *testing.T) {
	cfg := ConnectionConfig{Retry: RetryConfig{MaxRetries: 1}}
	opts, err := buildClientOptions(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts == nil {
		t.Fatal("expected non-nil options when RetryConfig is set")
	}
	if opts.RetryOptions.MaxRetries != 1 {
		t.Fatalf("RetryOptions.MaxRetries = %d, want 1", opts.RetryOptions.MaxRetries)
	}
}
