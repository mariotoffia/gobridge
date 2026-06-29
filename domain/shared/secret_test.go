package shared_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const rawSecret = "super-secret-value-123"

// TestSecret_Reveal verifies Reveal returns the exact raw value.
func TestSecret_Reveal(t *testing.T) {
	s := shared.NewSecret(rawSecret)
	if got := s.Reveal(); got != rawSecret {
		t.Fatalf("Reveal() = %q, want %q", got, rawSecret)
	}
}

// TestSecret_IsZero verifies IsZero reflects emptiness.
func TestSecret_IsZero(t *testing.T) {
	if !shared.NewSecret("").IsZero() {
		t.Error("empty secret should be zero")
	}
	if shared.NewSecret("x").IsZero() {
		t.Error("non-empty secret should not be zero")
	}
}

// TestSecret_Equal verifies value-based equality.
func TestSecret_Equal(t *testing.T) {
	a := shared.NewSecret("same")
	b := shared.NewSecret("same")
	c := shared.NewSecret("different")
	if !a.Equal(b) {
		t.Error("equal values should compare equal")
	}
	if a.Equal(c) {
		t.Error("different values should not compare equal")
	}
	if !shared.NewSecret("").Equal(shared.Secret{}) {
		t.Error("zero secret should equal empty-constructed secret")
	}
}

// TestSecret_String_Redacted verifies String redacts a populated secret and
// returns empty for the zero secret.
func TestSecret_String_Redacted(t *testing.T) {
	if got := shared.NewSecret(rawSecret).String(); got != "[REDACTED]" {
		t.Fatalf("String() = %q, want [REDACTED]", got)
	}
	if got := shared.NewSecret("").String(); got != "" {
		t.Fatalf("empty String() = %q, want \"\"", got)
	}
}

// TestSecret_GoString_Redacted verifies GoString mirrors String.
func TestSecret_GoString_Redacted(t *testing.T) {
	if got := shared.NewSecret(rawSecret).GoString(); got != "[REDACTED]" {
		t.Fatalf("GoString() = %q, want [REDACTED]", got)
	}
	if got := shared.NewSecret("").GoString(); got != "" {
		t.Fatalf("empty GoString() = %q, want \"\"", got)
	}
}

// TestSecret_Fmt_NeverLeaks verifies fmt verbs %v, %s, %#v never expose the value.
func TestSecret_Fmt_NeverLeaks(t *testing.T) {
	s := shared.NewSecret(rawSecret)
	for _, verb := range []string{"%v", "%s", "%#v", "%+v"} {
		out := fmt.Sprintf(verb, s)
		if strings.Contains(out, rawSecret) {
			t.Errorf("fmt %q leaked the secret: %q", verb, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("fmt %q did not redact: %q", verb, out)
		}
	}
}

// TestSecret_MarshalText_Redacts verifies MarshalText redacts by default so
// any generic TextMarshaler-based encoder cannot leak the value, and emits ""
// for the zero secret.
func TestSecret_MarshalText_Redacts(t *testing.T) {
	b, err := shared.NewSecret(rawSecret).MarshalText()
	if err != nil {
		t.Fatalf("MarshalText error: %v", err)
	}
	if string(b) != "[REDACTED]" {
		t.Fatalf("MarshalText = %q, want [REDACTED]", b)
	}
	empty, err := shared.NewSecret("").MarshalText()
	if err != nil {
		t.Fatalf("MarshalText(empty) error: %v", err)
	}
	if string(empty) != "" {
		t.Fatalf("MarshalText(empty) = %q, want \"\"", empty)
	}
}

// TestSecret_JSONMarshal_Redacts verifies json.Marshal of a struct embedding a
// Secret yields "[REDACTED]" by default — defense in depth so a stray
// json.Marshal anywhere (logs, admin reads, ad-hoc tooling) cannot leak the
// value. Decode of the PERSISTED form still produces a real Secret.
func TestSecret_JSONMarshal_Redacts(t *testing.T) {
	type wrapper struct {
		Name   string        `json:"name"`
		Secret shared.Secret `json:"secret"`
	}
	out, err := json.Marshal(wrapper{Name: "db", Secret: shared.NewSecret(rawSecret)})
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}
	if bytes.Contains(out, []byte(rawSecret)) {
		t.Errorf("json.Marshal leaked the secret: %s", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED]")) {
		t.Errorf("json.Marshal did not redact: %s", out)
	}

	// Round-trip via the PERSISTED form (a real scalar, as written by an
	// explicit Reveal at the save boundary) still decodes into a usable Secret.
	persisted := []byte(`{"name":"db","secret":"` + rawSecret + `"}`)
	var back wrapper
	if err := json.Unmarshal(persisted, &back); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if back.Secret.Reveal() != rawSecret {
		t.Errorf("round-trip of persisted form lost the value: got %q, want %q", back.Secret.Reveal(), rawSecret)
	}
}

// TestSecret_Revealed verifies the value-carried persistence escape hatch:
// a Revealed() copy emits the REAL value on json.Marshal while the original
// still redacts (no shared/global state), and display surfaces still redact
// even on the revealed copy.
func TestSecret_Revealed(t *testing.T) {
	s := shared.NewSecret(rawSecret)
	revealed := s.Revealed()

	out, err := json.Marshal(struct {
		S shared.Secret `json:"s"`
	}{revealed})
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}
	if !bytes.Contains(out, []byte(rawSecret)) {
		t.Errorf("Revealed() copy must emit the real value, got: %s", out)
	}

	// The original is untouched and still redacts.
	orig, _ := json.Marshal(struct {
		S shared.Secret `json:"s"`
	}{s})
	if bytes.Contains(orig, []byte(rawSecret)) {
		t.Errorf("Revealed() must not affect the original: %s", orig)
	}

	// Display/log surfaces redact even on a revealed copy.
	if revealed.String() != "[REDACTED]" {
		t.Errorf("revealed.String() = %q, want [REDACTED]", revealed.String())
	}
	if strings.Contains(fmt.Sprintf("%v", revealed), rawSecret) {
		t.Errorf("revealed copy leaked via fmt")
	}
}

// TestRevealSecrets_DeepCopy verifies the reflection reveal walk reveals nested
// Secrets (pointer + slice + map) for persistence while leaving the input
// untouched and redacting by default.
func TestRevealSecrets_DeepCopy(t *testing.T) {
	type inner struct {
		Key shared.Secret `json:"key"`
	}
	type outer struct {
		Name     string                   `json:"name"`
		Ptr      *inner                   `json:"ptr"`
		List     []inner                  `json:"list"`
		ByName   map[string]shared.Secret `json:"by_name"`
		NoSecret string                   `json:"no_secret"`
	}
	src := &outer{
		Name:     "cfg",
		Ptr:      &inner{Key: shared.NewSecret("p-secret")},
		List:     []inner{{Key: shared.NewSecret("l-secret")}},
		ByName:   map[string]shared.Secret{"a": shared.NewSecret("m-secret")},
		NoSecret: "visible",
	}

	revealed := shared.RevealSecrets(src)
	out, err := json.Marshal(revealed)
	if err != nil {
		t.Fatalf("json.Marshal revealed error: %v", err)
	}
	for _, want := range []string{"p-secret", "l-secret", "m-secret", "visible"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("revealed deep copy missing %q: %s", want, out)
		}
	}

	// Input untouched: a default marshal of src still redacts every secret.
	orig, _ := json.Marshal(src)
	for _, secret := range []string{"p-secret", "l-secret", "m-secret"} {
		if bytes.Contains(orig, []byte(secret)) {
			t.Errorf("RevealSecrets mutated the input or default leaked %q: %s", secret, orig)
		}
	}
}

// TestSecret_UnmarshalText_RoundTrip verifies UnmarshalText stores the real
// value, reachable via Reveal, including through json.Unmarshal of a string.
func TestSecret_UnmarshalText_RoundTrip(t *testing.T) {
	var s shared.Secret
	if err := s.UnmarshalText([]byte(rawSecret)); err != nil {
		t.Fatalf("UnmarshalText error: %v", err)
	}
	if s.Reveal() != rawSecret {
		t.Fatalf("after UnmarshalText, Reveal() = %q, want %q", s.Reveal(), rawSecret)
	}

	var via shared.Secret
	if err := json.Unmarshal([]byte(`"`+rawSecret+`"`), &via); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if via.Reveal() != rawSecret {
		t.Fatalf("after json.Unmarshal, Reveal() = %q, want %q", via.Reveal(), rawSecret)
	}
}

// TestSecret_LogValue_Redacted verifies slog rendering redacts the value.
func TestSecret_LogValue_Redacted(t *testing.T) {
	s := shared.NewSecret(rawSecret)
	if v := s.LogValue(); v.String() != "[REDACTED]" {
		t.Fatalf("LogValue() = %q, want [REDACTED]", v.String())
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("auth", slog.Any("secret", s))
	if strings.Contains(buf.String(), rawSecret) {
		t.Errorf("slog output leaked the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Errorf("slog output did not redact: %s", buf.String())
	}
}
