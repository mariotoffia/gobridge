package connectivity_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// TestCredentialKind_Constants validates that CredentialPassword and CredentialTLS
// are distinct, non-empty string values.
func TestCredentialKind_Constants(t *testing.T) {
	kinds := []connectivity.CredentialKind{
		connectivity.CredentialPassword,
		connectivity.CredentialTLS,
	}
	seen := make(map[connectivity.CredentialKind]bool, len(kinds))
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

// TestCredentialSet_ZeroValue validates that a zero-value CredentialSet has nil Password and TLS accessors.
func TestCredentialSet_ZeroValue(t *testing.T) {
	var cs connectivity.CredentialSet
	if cs.Password() != nil {
		t.Fatal("zero-value CredentialSet.Password should be nil")
	}
	if cs.TLS() != nil {
		t.Fatal("zero-value CredentialSet.TLS should be nil")
	}
}

// TestPasswordCredential_Accessors validates the constructor and accessors,
// including that the password is exposed only via Reveal.
func TestPasswordCredential_Accessors(t *testing.T) {
	pc := connectivity.NewPasswordCredential("admin", "s3cret")
	if pc.Username() != "admin" {
		t.Fatalf("Username(): got %q, want %q", pc.Username(), "admin")
	}
	if pc.Password().Reveal() != "s3cret" {
		t.Fatalf("Password().Reveal(): got %q, want %q", pc.Password().Reveal(), "s3cret")
	}
	if pc.Password().String() != "[REDACTED]" {
		t.Fatalf("Password().String(): got %q, want [REDACTED]", pc.Password().String())
	}
}

// TestTLSMaterial_Accessors validates the constructor and accessors including
// the CAPEMs bundle and InsecureSkipVerify flag.
func TestTLSMaterial_Accessors(t *testing.T) {
	tls := connectivity.NewTLSMaterial("cert-data", "key-data", []string{"ca1", "ca2"}, true)
	if tls.CertPEM() != "cert-data" {
		t.Fatalf("CertPEM(): got %q, want %q", tls.CertPEM(), "cert-data")
	}
	if tls.KeyPEM().Reveal() != "key-data" {
		t.Fatalf("KeyPEM().Reveal(): got %q, want %q", tls.KeyPEM().Reveal(), "key-data")
	}
	cas := tls.CAPEMs()
	if len(cas) != 2 || cas[0] != "ca1" || cas[1] != "ca2" {
		t.Fatalf("CAPEMs(): got %v, want [ca1 ca2]", cas)
	}
	if !tls.InsecureSkipVerify() {
		t.Fatal("InsecureSkipVerify() should be true")
	}
}

// TestTLSMaterial_DefensiveCopy_OnConstruction verifies that mutating the
// slice handed to NewTLSMaterial after construction does NOT alter the stored
// material — the constructor must copy the CA bundle.
func TestTLSMaterial_DefensiveCopy_OnConstruction(t *testing.T) {
	src := []string{"ca-original"}
	tls := connectivity.NewTLSMaterial("c", "k", src, false)
	src[0] = "ca-tampered"
	if got := tls.CAPEMs(); got[0] != "ca-original" {
		t.Fatalf("mutating the source slice leaked into the VO: got %q", got[0])
	}
}

// TestTLSMaterial_CAPEMs_ReturnsCopy verifies the caller cannot mutate the
// persisted material through the slice returned by CAPEMs: mutate the
// returned copy, re-read, and confirm the stored value is unchanged.
func TestTLSMaterial_CAPEMs_ReturnsCopy(t *testing.T) {
	tls := connectivity.NewTLSMaterial("c", "k", []string{"ca-original"}, false)
	got := tls.CAPEMs()
	got[0] = "ca-tampered"
	if reread := tls.CAPEMs(); reread[0] != "ca-original" {
		t.Fatalf("mutating the returned CAPEMs leaked into the VO: got %q", reread[0])
	}
}

// TestCredentialSet_Equal_ValueBased verifies Equal compares by value across
// freshly-allocated credential sets built via the constructors.
func TestCredentialSet_Equal_ValueBased(t *testing.T) {
	mk := func() *connectivity.CredentialSet {
		pw := connectivity.NewPasswordCredential("u", "p")
		tls := connectivity.NewTLSMaterial("cert", "key", []string{"ca"}, true)
		return connectivity.NewCredentialSet(&pw, &tls)
	}
	a, b := mk(), mk()
	if !a.Equal(b) {
		t.Fatal("identical credential sets should compare equal")
	}

	pwDiff := connectivity.NewPasswordCredential("u", "different")
	c := connectivity.NewCredentialSet(&pwDiff, nil)
	if a.Equal(c) {
		t.Fatal("credential sets with different passwords should not compare equal")
	}

	tlsDiff := connectivity.NewTLSMaterial("cert", "key", []string{"ca", "ca2"}, true)
	pw := connectivity.NewPasswordCredential("u", "p")
	d := connectivity.NewCredentialSet(&pw, &tlsDiff)
	if a.Equal(d) {
		t.Fatal("credential sets with different CA bundles should not compare equal")
	}
}

// TestCredentialSet_AccessorsReturnStoredPointers verifies pointer identity
// from the accessors, nil-field nil-return, and nil-receiver safety.
func TestCredentialSet_AccessorsReturnStoredPointers(t *testing.T) {
	pw := connectivity.NewPasswordCredential("u", "p")
	tls := connectivity.NewTLSMaterial("c", "k", []string{"ca"}, false)

	cs := connectivity.NewCredentialSet(&pw, &tls)
	if cs.Password() != &pw {
		t.Fatal("Password() must return the stored pointer, not a copy")
	}
	if cs.TLS() != &tls {
		t.Fatal("TLS() must return the stored pointer, not a copy")
	}

	// nil-field nil-return
	if connectivity.NewCredentialSet(&pw, nil).TLS() != nil {
		t.Fatal("TLS() must be nil when constructed with nil tls")
	}
	if connectivity.NewCredentialSet(nil, &tls).Password() != nil {
		t.Fatal("Password() must be nil when constructed with nil password")
	}

	// nil-receiver safety — must not panic
	var z *connectivity.CredentialSet
	if z.Password() != nil {
		t.Fatal("nil-receiver Password() must return nil")
	}
	if z.TLS() != nil {
		t.Fatal("nil-receiver TLS() must return nil")
	}
}

// TestCredentials_Redacted_InFormatting verifies neither VO leaks secret
// material through fmt verbs even when nested inside a CredentialSet.
func TestCredentials_Redacted_InFormatting(t *testing.T) {
	pw := connectivity.NewPasswordCredential("admin", "super-secret-password-123")
	tls := connectivity.NewTLSMaterial(
		"-----BEGIN CERTIFICATE-----\ncert",
		"-----BEGIN PRIVATE KEY-----\nkey",
		[]string{"ca"},
		false,
	)
	set := connectivity.NewCredentialSet(&pw, &tls)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, set)
		if strings.Contains(out, "super-secret-password-123") {
			t.Errorf("fmt %q leaked the password: %s", verb, out)
		}
		if strings.Contains(out, "BEGIN PRIVATE KEY") {
			t.Errorf("fmt %q leaked the private key: %s", verb, out)
		}
	}
}
