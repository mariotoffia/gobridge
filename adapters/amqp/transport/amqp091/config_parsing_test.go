// ═══════════════════════════════════════════════
// Config Option Parsing Tests
//
// Validates BUG-3: amqp091 optDuration/optInt only accept
// native Go types, rejecting JSON-sourced float64/string values.
//
// Also validates API-1: SessionOptionsFromMap should call
// validate() like amqp10 does.
// ═══════════════════════════════════════════════
package amqp091

import (
	"testing"
	"time"
)

// TestOptDuration_NativeDuration validates the existing time.Duration path.
func TestOptDuration_NativeDuration(t *testing.T) {
	m := map[string]any{"key": 5 * time.Second}
	d, ok := optDuration(m, "key")
	if !ok {
		t.Fatal("expected ok=true for time.Duration value")
	}
	if d != 5*time.Second {
		t.Fatalf("duration = %v, want 5s", d)
	}
}

// TestOptDuration_StringParsing exposes BUG-3: optDuration should parse
// duration strings like "5s", "100ms" (as JSON/YAML typically produce).
func TestOptDuration_StringParsing(t *testing.T) {
	m := map[string]any{"key": "5s"}
	d, ok := optDuration(m, "key")
	if !ok {
		t.Fatal("expected ok=true for duration string '5s' — " +
			"BUG-3: amqp091 optDuration only accepts time.Duration, not string")
	}
	if d != 5*time.Second {
		t.Fatalf("duration = %v, want 5s", d)
	}
}

// TestOptDuration_Float64 exposes BUG-3: JSON numbers decode as float64.
func TestOptDuration_Float64(t *testing.T) {
	m := map[string]any{"key": float64(10)}
	d, ok := optDuration(m, "key")
	if !ok {
		t.Fatal("expected ok=true for float64 — " +
			"BUG-3: amqp091 optDuration does not handle float64 from JSON")
	}
	if d != 10*time.Second {
		t.Fatalf("duration = %v, want 10s", d)
	}
}

// TestOptDuration_Int exposes BUG-3: int seconds should be supported.
func TestOptDuration_Int(t *testing.T) {
	m := map[string]any{"key": 30}
	d, ok := optDuration(m, "key")
	if !ok {
		t.Fatal("expected ok=true for int — BUG-3: amqp091 optDuration does not handle int")
	}
	if d != 30*time.Second {
		t.Fatalf("duration = %v, want 30s", d)
	}
}

// TestOptInt_Float64 exposes BUG-3: JSON numbers decode as float64.
func TestOptInt_Float64(t *testing.T) {
	m := map[string]any{"key": float64(42)}
	v, ok := optInt(m, "key")
	if !ok {
		t.Fatal("expected ok=true for float64 — BUG-3: optInt does not handle float64")
	}
	if v != 42 {
		t.Fatalf("value = %d, want 42", v)
	}
}

// TestOptInt_NativeInt validates existing int path works.
func TestOptInt_NativeInt(t *testing.T) {
	m := map[string]any{"key": 7}
	v, ok := optInt(m, "key")
	if !ok {
		t.Fatal("expected ok=true for int")
	}
	if v != 7 {
		t.Fatalf("value = %d, want 7", v)
	}
}

// TestOptInt_Missing validates missing key returns false.
func TestOptInt_Missing(t *testing.T) {
	m := map[string]any{}
	_, ok := optInt(m, "missing")
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

// TestSessionOptionsFromMap_Validates exposes API-1: SessionOptionsFromMap
// should validate the options (e.g., reject empty broker_url).
func TestSessionOptionsFromMap_Validates(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{})
	if err == nil {
		t.Fatal("SessionOptionsFromMap should validate and return error " +
			"for missing broker_url (API-1: no validation)")
	}
}

// TestSessionOptionsFromMap_ValidBrokerURL validates happy path.
func TestSessionOptionsFromMap_ValidBrokerURL(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"broker_url": "amqp://localhost:5672/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.BrokerURL != "amqp://localhost:5672/" {
		t.Fatalf("BrokerURL = %q", opts.BrokerURL)
	}
}

// TestSessionOptionsFromMap_AllFields validates all option keys.
func TestSessionOptionsFromMap_AllFields(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"broker_url":      "amqp://host:5672/",
		"username":        "admin",
		"password":        "secret",
		"vhost":           "production",
		"heartbeat":       10 * time.Second,
		"connect_timeout": 5 * time.Second,
		"reconnect_delay": 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Username != "admin" {
		t.Errorf("Username = %q", opts.Username)
	}
	if opts.Password != "secret" {
		t.Errorf("Password = %q", opts.Password)
	}
	if opts.Vhost != "production" {
		t.Errorf("Vhost = %q", opts.Vhost)
	}
}

// TestSessionOptionsFromMap_TLSFromMap validates TLS config parsed from nested map.
func TestSessionOptionsFromMap_TLSFromMap(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"broker_url": "amqps://host:5671/",
		"tls": map[string]any{
			"enable":               true,
			"insecure_skip_verify": true,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.TLS == nil {
		t.Fatal("TLS config should be non-nil")
	}
	if !opts.TLS.Enable {
		t.Error("TLS.Enable should be true")
	}
	if !opts.TLS.InsecureSkipVerify {
		t.Error("TLS.InsecureSkipVerify should be true")
	}
}

// TestReceiverConfigFromOptions_Defaults validates default PrefetchCount.
func TestReceiverConfigFromOptions_Defaults(t *testing.T) {
	cfg := ReceiverConfigFromOptions(nil)
	if cfg.PrefetchCount != 10 {
		t.Fatalf("PrefetchCount = %d, want 10", cfg.PrefetchCount)
	}
}

// TestSenderConfigFromOptions_DefaultTimeout validates default timeout.
func TestSenderConfigFromOptions_DefaultTimeout(t *testing.T) {
	cfg := SenderConfigFromOptions(nil)
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", cfg.Timeout)
	}
}
