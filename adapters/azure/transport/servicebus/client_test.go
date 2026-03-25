package servicebus

import (
	"testing"
)

// TestBuildTLSConfig_Nil validates that buildTLSConfig returns nil when
// neither CA PEM nor InsecureSkipVerify is set.
func TestBuildTLSConfig_Nil(t *testing.T) {
	tc := buildTLSConfig("", false)
	if tc != nil {
		t.Fatal("expected nil tls.Config when no CA PEM and no insecure flag")
	}
}

// TestBuildTLSConfig_InsecureSkipVerify validates that the InsecureSkipVerify
// flag is propagated to the resulting tls.Config.
func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	tc := buildTLSConfig("", true)
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
	// Use a real self-signed CA cert PEM for testing.
	caPEM := `-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJALRiMLAh9EvPMA0GCSqGSIb3DQEBCwUAMBExDzANBgNVBAMMBnRl
c3RjYTAeFw0yNTAxMDEwMDAwMDBaFw0yNjAxMDEwMDAwMDBaMBExDzANBgNVBAMM
BnRlc3RjYTBcMA0GCSqGSIb3DQEBAQUAA0sAMEgCQQC7o96RsV5UxL/3BGZY8i7F
DpBfz1yk6hDIGcMT0d6rU5HdGCqqPHI7+Lr9bNJXv1HCWDdKOfFqE0KCJmU+nLrA
gMBAAEwDQYJKoZIhvcNAQELBQADQQA3R3s8GYTaa3F+L06G9ZFWJpCuZ7k+wMOE
By1VNFHGN9VnfN4I/TKqD3ObiSbZ+9u/CjKzYPvD5T7N0VjcVaZQ
-----END CERTIFICATE-----`

	tc := buildTLSConfig(caPEM, false)
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

// TestBuildClientOptions_NoTLS validates that buildClientOptions returns nil
// when no TLS configuration is needed.
func TestBuildClientOptions_NoTLS(t *testing.T) {
	cfg := ConnectionConfig{}
	opts := buildClientOptions(cfg)
	if opts != nil {
		t.Fatal("expected nil options when no TLS config is set")
	}
}

// TestBuildClientOptions_WithTLSConfig validates that an explicit TLSConfig
// on ConnectionConfig is used in the returned client options.
func TestBuildClientOptions_WithTLSConfig(t *testing.T) {
	tc := buildTLSConfig("", true)
	cfg := ConnectionConfig{TLSConfig: tc}
	opts := buildClientOptions(cfg)
	if opts == nil {
		t.Fatal("expected non-nil options when TLSConfig is explicitly set")
	}
	if opts.TLSConfig != tc {
		t.Fatal("TLSConfig should be the provided instance")
	}
}
