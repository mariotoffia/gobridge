package connectivity_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════
// Credential Redaction Tests
//
// Validates that credential value objects do not leak sensitive values
// when formatted with fmt verbs (%v, %+v, %s, %q, %#v).
//
// Issue SEC-001: PasswordCredential and TLSMaterial must redact through
// String()/GoString(); their secret fields are private and wrapped in
// shared.Secret so Go's default formatting cannot expose them.
// ═══════════════════════════════════════════════════════════════════

// TestPasswordCredential_String_Redacted validates that String() returns
// a redacted representation that does not contain the actual password.
func TestPasswordCredential_String_Redacted(t *testing.T) {
	pc := connectivity.NewPasswordCredential("admin", "super-secret-password-123")

	s := fmt.Sprintf("%v", pc)

	if strings.Contains(s, "super-secret-password-123") {
		t.Fatalf("password leaked in %%v formatting: %s", s)
	}
	if strings.Contains(s, pc.Password().Reveal()) {
		t.Fatalf("password value appeared in formatted output: %s", s)
	}
}

// TestPasswordCredential_GoString_Redacted validates that GoString()
// does not contain the actual password value.
func TestPasswordCredential_GoString_Redacted(t *testing.T) {
	pc := connectivity.NewPasswordCredential("admin", "super-secret-password-123")

	s := fmt.Sprintf("%#v", pc)

	if strings.Contains(s, "super-secret-password-123") {
		t.Fatalf("password leaked in %%#v formatting: %s", s)
	}
}

// TestTLSMaterial_String_Redacted validates that String() does not
// expose certificate or key PEM material.
func TestTLSMaterial_String_Redacted(t *testing.T) {
	tls := connectivity.NewTLSMaterial(
		"-----BEGIN CERTIFICATE-----\nMIIBxTCCAW...",
		"-----BEGIN PRIVATE KEY-----\nMIIEvgIBADA...",
		[]string{"-----BEGIN CERTIFICATE-----\nCA-CERT..."},
		false,
	)

	s := fmt.Sprintf("%v", tls)

	if strings.Contains(s, "BEGIN CERTIFICATE") {
		t.Fatalf("certificate PEM leaked in %%v formatting: %s", s)
	}
	if strings.Contains(s, "BEGIN PRIVATE KEY") {
		t.Fatalf("private key PEM leaked in %%v formatting: %s", s)
	}
}

// TestTLSMaterial_GoString_Redacted validates that GoString() does not
// expose PEM material.
func TestTLSMaterial_GoString_Redacted(t *testing.T) {
	tls := connectivity.NewTLSMaterial(
		"-----BEGIN CERTIFICATE-----\ndata...",
		"-----BEGIN PRIVATE KEY-----\ndata...",
		nil,
		false,
	)

	s := fmt.Sprintf("%#v", tls)

	if strings.Contains(s, "BEGIN PRIVATE KEY") {
		t.Fatalf("private key leaked in %%#v formatting: %s", s)
	}
}
