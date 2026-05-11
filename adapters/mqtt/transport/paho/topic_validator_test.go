package paho

import (
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// MQTT topic validator tests — moved here from runtime/ as part of the
// AP-005 promotion of address validation to a transport capability.
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

func TestValidateMQTTTopic_EmptySegment(t *testing.T) {
	if err := ValidateMQTTTopic("devices//data"); err == nil {
		t.Fatal("empty segment should be rejected")
	}
}

func TestValidateMQTTTopic_LeadingSlash(t *testing.T) {
	if err := ValidateMQTTTopic("/devices/data"); err == nil {
		t.Fatal("leading slash (empty first segment) should be rejected")
	}
}

func TestValidateMQTTTopic_TrailingSlash(t *testing.T) {
	if err := ValidateMQTTTopic("devices/data/"); err == nil {
		t.Fatal("trailing slash (empty last segment) should be rejected")
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

// TestValidateMQTTTopic_DollarPrefix validates the $-prefix reserved
// topic guard (MQTT v5 §4.7.2).
func TestValidateMQTTTopic_DollarPrefix(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"$SYS prefix", "$SYS/broker/load", true},
		{"$share prefix", "$share/group/topic", true},
		{"$ alone", "$", true},
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
		{"empty segment", "devices//temp", true},
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
