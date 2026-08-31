package paho

import (
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// MQTT topic validator tests — moved here from runtime/ as part of the
// promotion of address validation to a transport capability.
// The runtime no longer owns MQTT topic semantics; the paho factory
// exposes the validator via NewAddressValidator() and the runtime
// dispatches generically per binding.
// ─────────────────────────────────────────────────────────────────────

func TestValidateMQTTTopic_ValidTopics(t *testing.T) {
	valid := []string{
		"devices/sensor-1/data",
		"factory/a/orders/42",
		"a",
		"a/b/c/d/e",
	}
	for _, topic := range valid {
		if err := ValidateMQTTTopic(topic); err != nil {
			t.Errorf("topic %q should be valid: %v", topic, err)
		}
	}
}

func TestValidateMQTTTopic_Empty(t *testing.T) {
	if err := ValidateMQTTTopic(""); err == nil {
		t.Fatal("empty topic should be rejected")
	}
}

func TestValidateMQTTTopic_PlusWildcard(t *testing.T) {
	if err := ValidateMQTTTopic("devices/+/data"); err == nil {
		t.Fatal("plus wildcard should be rejected")
	}
}

func TestValidateMQTTTopic_HashWildcard(t *testing.T) {
	if err := ValidateMQTTTopic("devices/#"); err == nil {
		t.Fatal("hash wildcard should be rejected")
	}
}

func TestValidateMQTTTopic_NullCharacter(t *testing.T) {
	if err := ValidateMQTTTopic("devices/\x00/data"); err == nil {
		t.Fatal("null character should be rejected")
	}
}

func TestValidateMQTTTopicFilter(t *testing.T) {
	tests := []struct {
		filter string
		valid  bool
	}{
		{filter: "#", valid: true},
		{filter: "orders/+/created", valid: true},
		{filter: "$SYS/broker/+", valid: true},
		{filter: "$share/workers/orders/#", valid: true},
		{filter: "", valid: false},
		{filter: "orders/#/dead", valid: false},
		{filter: "orders/new+", valid: false},
		{filter: "$share//orders/#", valid: false},
		{filter: "$share/workers/", valid: false},
		{filter: "$share/work+ers/orders/#", valid: false},
		{filter: "orders/\x00", valid: false},
		{filter: string([]byte{0xff}), valid: false},
	}
	for _, tc := range tests {
		err := ValidateMQTTTopicFilter(tc.filter)
		if tc.valid && err != nil {
			t.Errorf("ValidateMQTTTopicFilter(%q) = %v, want valid", tc.filter, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("ValidateMQTTTopicFilter(%q) = nil, want invalid", tc.filter)
		}
	}
}

// TestValidateMQTTTopic_EmptySegment pins: empty topic levels are
// spec-legal for a publish Topic Name (MQTT 5.0 §4.7.1.1 — only the whole
// name must be non-empty) and must be accepted. Real devices emit "a//b" and
// a mirror route re-publishing such a source topic must not DLQ it.
func TestValidateMQTTTopic_EmptySegment(t *testing.T) {
	if err := ValidateMQTTTopic("devices//data"); err != nil {
		t.Fatalf("empty middle segment is spec-legal and must be accepted: %v", err)
	}
	if err := ValidateMQTTTopic("/"); err != nil {
		t.Fatalf("a lone %q is an explicitly valid MQTT topic: %v", "/", err)
	}
}

// TestValidateMQTTTopic_LeadingSlash pins: a leading empty level
// ("/devices/data") is a distinct, legal MQTT topic and must be accepted.
func TestValidateMQTTTopic_LeadingSlash(t *testing.T) {
	if err := ValidateMQTTTopic("/devices/data"); err != nil {
		t.Fatalf("leading slash (empty first level) is spec-legal: %v", err)
	}
}

// TestValidateMQTTTopic_TrailingSlash pins: a trailing empty level
// ("devices/data/") is a distinct, legal MQTT topic and must be accepted.
func TestValidateMQTTTopic_TrailingSlash(t *testing.T) {
	if err := ValidateMQTTTopic("devices/data/"); err != nil {
		t.Fatalf("trailing slash (empty last level) is spec-legal: %v", err)
	}
}

// TestValidateMQTTTopic_MaxLength validates that topics exceeding the
// MQTT v5 spec ceiling (65,535 bytes) are rejected.
func TestValidateMQTTTopic_MaxLength(t *testing.T) {
	longTopic := strings.Repeat("a", 65536)
	err := ValidateMQTTTopic(longTopic)
	if err == nil {
		t.Fatal("expected error for topic exceeding 65,535 bytes")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("error should mention length, got: %v", err)
	}
}

// TestValidateMQTTTopic_ExactMaxLength validates the boundary.
func TestValidateMQTTTopic_ExactMaxLength(t *testing.T) {
	topic := strings.Repeat("a", 65535)
	if err := ValidateMQTTTopic(topic); err != nil {
		t.Fatalf("topic of exactly 65,535 bytes should be valid, got: %v", err)
	}
}

// TestValidateMQTTTopic_DollarNamespaces pins which $-prefixed publish topics
// are structurally rejected. MQTT v5 §4.7.2 reserves the $ prefix for the
// SERVER to define; it does not make publishing to one a syntax error, and
// real brokers define legal write namespaces there — AWS IoT's $aws/rules/…
// republish target is the canonical one. Rejecting the whole prefix
// terminalized those messages inside the bridge before the broker ever saw
// them, so only $share/ — a filter-only construct that can never name a
// publish destination — stays rejected. Everything else is the broker's
// authorization decision, and its denial comes back as a PUBACK reason code.
func TestValidateMQTTTopic_DollarNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"aws iot rules republish", "$aws/rules/myrule", false},
		{"aws iot shadow update", "$aws/things/sensor-1/shadow/update", false},
		{"$SYS prefix", "$SYS/broker/load", false},
		{"$ alone", "$", false},
		{"$share prefix", "$share/group/topic", true},
		{"dollar mid-topic", "devices/$status", false},
		{"normal topic", "devices/sensor/temp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMQTTTopic(tt.topic)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for topic %q", tt.topic)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for topic %q: %v", tt.topic, err)
			}
		})
	}
}

// TestValidateMQTTTopic_ExistingValidation is a regression guard.
func TestValidateMQTTTopic_ExistingValidation(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"empty", "", true},
		{"wildcard +", "devices/+/temp", true},
		{"wildcard #", "devices/#", true},
		{"null byte", "devices/\x00/temp", true},
		{"empty segment (spec-legal)", "devices//temp", false},
		{"valid", "devices/sensor/temp", false},
		{"single segment", "topic", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMQTTTopic(tt.topic)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for topic %q", tt.topic)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for topic %q: %v", tt.topic, err)
			}
		})
	}
}

// TestNewAddressValidator_Delegates checks the ports.AddressValidator
// wrapper returned by NewAddressValidator delegates to ValidateMQTTTopic.
func TestNewAddressValidator_Delegates(t *testing.T) {
	v := NewAddressValidator()
	if v == nil {
		t.Fatal("NewAddressValidator returned nil")
	}
	if err := v.ValidateAddress("devices/sensor/temp"); err != nil {
		t.Fatalf("valid topic rejected by validator: %v", err)
	}
	if err := v.ValidateAddress("devices/+/temp"); err == nil {
		t.Fatal("validator must reject + wildcard")
	}
}

// TestFactory_AddressValidator confirms the paho factory advertises a
// non-nil validator (the seam that runtime relies on).
func TestFactory_AddressValidator(t *testing.T) {
	f := NewFactory(nil)
	if f.AddressValidator() == nil {
		t.Fatal("paho factory must expose a non-nil AddressValidator")
	}
}

// FuzzValidateMQTTTopic mirrors the previous runtime fuzz seed set.
func FuzzValidateMQTTTopic(f *testing.F) {
	f.Add("devices/sensor/temp")
	f.Add("")
	f.Add("a/+/b")
	f.Add("#")
	f.Add("a//b")
	f.Add(string([]byte{0}))
	f.Add("very/deep/topic/with/many/segments/a/b/c/d")

	f.Fuzz(func(t *testing.T, topic string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = ValidateMQTTTopic(topic)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("ValidateMQTTTopic did not return within 100ms for topic=%q", topic)
		}
	})
}
