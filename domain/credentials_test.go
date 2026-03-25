package domain_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// TestCredentialKind_Constants validates that CredentialPassword and CredentialTLS
// are distinct, non-empty string values.
func TestCredentialKind_Constants(t *testing.T) {
	kinds := []domain.CredentialKind{
		domain.CredentialPassword,
		domain.CredentialTLS,
	}
	seen := make(map[domain.CredentialKind]bool, len(kinds))
	for _, k := range kinds {
		if k == "" {
			t.Fatal("credential kind constant must not be empty")
		}
		if seen[k] {
			t.Fatalf("duplicate credential kind: %q", k)
		}
		seen[k] = true
	}
}

// TestCredentialSet_ZeroValue validates that a zero-value CredentialSet has nil Password and TLS fields.
func TestCredentialSet_ZeroValue(t *testing.T) {
	var cs domain.CredentialSet
	if cs.Password != nil {
		t.Fatal("zero-value CredentialSet.Password should be nil")
	}
	if cs.TLS != nil {
		t.Fatal("zero-value CredentialSet.TLS should be nil")
	}
}

// TestPasswordCredential_Fields validates field assignment on PasswordCredential.
func TestPasswordCredential_Fields(t *testing.T) {
	pc := domain.PasswordCredential{
		Username: "admin",
		Password: "s3cret",
	}
	if pc.Username != "admin" {
		t.Fatalf("Username: got %q, want %q", pc.Username, "admin")
	}
	if pc.Password != "s3cret" {
		t.Fatalf("Password: got %q, want %q", pc.Password, "s3cret")
	}
}

// TestTLSMaterial_Fields validates field assignment on TLSMaterial including CAPEMs slice
// and InsecureSkipVerify flag.
func TestTLSMaterial_Fields(t *testing.T) {
	tls := domain.TLSMaterial{
		CertPEM:            "cert-data",
		KeyPEM:             "key-data",
		CAPEMs:             []string{"ca1", "ca2"},
		InsecureSkipVerify: true,
	}
	if tls.CertPEM != "cert-data" {
		t.Fatalf("CertPEM: got %q, want %q", tls.CertPEM, "cert-data")
	}
	if tls.KeyPEM != "key-data" {
		t.Fatalf("KeyPEM: got %q, want %q", tls.KeyPEM, "key-data")
	}
	if len(tls.CAPEMs) != 2 || tls.CAPEMs[0] != "ca1" || tls.CAPEMs[1] != "ca2" {
		t.Fatalf("CAPEMs: got %v, want [ca1 ca2]", tls.CAPEMs)
	}
	if !tls.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be true")
	}
}
