// ═══════════════════════════════════════════════
// TLS Configuration & Routing Type Tests
//
// Validates BuildTLSConfig, RoutingType.capability(),
// and option parser edge cases.
// ═══════════════════════════════════════════════
package amqp10

import (
	"testing"
)

// TestBuildTLSConfig_Nil validates nil config returns nil.
func TestBuildTLSConfig_Nil(t *testing.T) {
	cfg, err := BuildTLSConfig(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil tls.Config for nil input")
	}
}

// TestBuildTLSConfig_Disabled validates disabled TLS returns nil.
func TestBuildTLSConfig_Disabled(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{Enable: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil tls.Config for disabled TLS")
	}
}

// TestBuildTLSConfig_InsecureSkipVerify validates the flag is set correctly.
func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{
		Enable:             true,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be true")
	}
}

// TestBuildTLSConfig_MinVersion validates TLS 1.2 minimum.
func TestBuildTLSConfig_MinVersion(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{Enable: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinVersion != 0x0303 { // tls.VersionTLS12
		t.Fatalf("MinVersion = %x, want 0x0303 (TLS 1.2)", cfg.MinVersion)
	}
}

// TestBuildTLSConfig_BadCACert validates error on non-existent CA file.
func TestBuildTLSConfig_BadCACert(t *testing.T) {
	_, err := BuildTLSConfig(&TLSConfig{
		Enable:     true,
		CACertFile: "/nonexistent/ca.pem",
	})
	if err == nil {
		t.Fatal("expected error for non-existent CA cert file")
	}
}

// TestBuildTLSConfig_BadKeyPair validates error on non-existent cert files.
func TestBuildTLSConfig_BadKeyPair(t *testing.T) {
	_, err := BuildTLSConfig(&TLSConfig{
		Enable:   true,
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
	})
	if err == nil {
		t.Fatal("expected error for non-existent key pair")
	}
}

// TestRoutingType_Capability validates anycast→queue, multicast→topic.
func TestRoutingType_Capability(t *testing.T) {
	tests := []struct {
		rt   RoutingType
		want string
	}{
		{RoutingAnycast, "queue"},
		{RoutingMulticast, "topic"},
		{RoutingType(99), "queue"}, // unknown defaults to queue
	}

	for _, tc := range tests {
		got := tc.rt.capability()
		if got != tc.want {
			t.Errorf("RoutingType(%d).capability() = %q, want %q", tc.rt, got, tc.want)
		}
	}
}

// TestReceiverConfigFromOptions_Multicast validates routing option parsing.
func TestReceiverConfigFromOptions_Multicast(t *testing.T) {
	m := map[string]any{
		"address": "topic://events",
		"routing": "multicast",
	}
	cfg := ReceiverConfigFromOptions(m)
	if cfg.Address != "topic://events" {
		t.Errorf("Address = %q", cfg.Address)
	}
	if cfg.Routing != RoutingMulticast {
		t.Errorf("Routing = %d, want %d (RoutingMulticast)", cfg.Routing, RoutingMulticast)
	}
}

// TestSenderConfigFromOptions_Multicast validates routing option parsing.
func TestSenderConfigFromOptions_Multicast(t *testing.T) {
	m := map[string]any{
		"address": "topic://events",
		"routing": "multicast",
	}
	cfg := SenderConfigFromOptions(m)
	if cfg.Address != "topic://events" {
		t.Errorf("Address = %q", cfg.Address)
	}
	if cfg.Routing != RoutingMulticast {
		t.Errorf("Routing = %d, want %d (RoutingMulticast)", cfg.Routing, RoutingMulticast)
	}
}

// TestOptUint32_AllTypes validates parsing of int32, uint32, float64 types.
func TestOptUint32_AllTypes(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		wantVal uint32
		wantOK  bool
	}{
		{"int32_positive", int32(42), 42, true},
		{"int32_negative", int32(-1), 0, false},
		{"uint32", uint32(100), 100, true},
		{"float64_positive", float64(3.14), 3, true},
		{"float64_negative", float64(-1.0), 0, false},
		{"string_unsupported", "42", 0, false},
		{"bool_unsupported", true, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"key": tc.value}
			val, ok := optUint32(m, "key")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if val != tc.wantVal {
				t.Fatalf("val = %d, want %d", val, tc.wantVal)
			}
		})
	}
}

// TestOptDuration_StringParsing validates duration string parsing.
func TestOptDuration_StringParsing(t *testing.T) {
	m := map[string]any{"key": "5s"}
	d, ok := optDuration(m, "key")
	if !ok {
		t.Fatal("expected ok=true for valid duration string")
	}
	if d != 5*1e9 { // 5 seconds in nanoseconds
		t.Fatalf("duration = %v, want 5s", d)
	}
}

// TestOptDuration_InvalidString validates that invalid strings return false.
func TestOptDuration_InvalidString(t *testing.T) {
	m := map[string]any{"key": "not-a-duration"}
	_, ok := optDuration(m, "key")
	if ok {
		t.Fatal("expected ok=false for invalid duration string")
	}
}
