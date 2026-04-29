package config

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

func minimalValidBridgeConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Receivers: []ports.ReceiverDef{
			{ID: "r1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "s1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "s1", Address: "q1"},
		},
		Routes: []ports.RouteDef{
			{ID: "route1", ReceiverID: "r1", Bindings: []string{"b1"}},
		},
	}
}

func assertDirectHoldFencingWarning(t *testing.T, warnings []string) {
	t.Helper()
	if len(warnings) == 0 {
		t.Fatal("expected at least one validation warning")
	}
	combined := strings.Join(warnings, " ")
	if !strings.Contains(combined, "direct_hold") {
		t.Fatalf("warning should mention direct_hold, got: %v", warnings)
	}
	if !strings.Contains(combined, "fencing") {
		t.Fatalf("warning should mention fencing, got: %v", warnings)
	}
}

// TestValidateWithWarnings_DirectHoldEmitsWarning verifies that a valid config
// with delivery_mode="direct_hold" passes validation but emits a fencing warning.
func TestValidateWithWarnings_DirectHoldEmitsWarning(t *testing.T) {
	cfg := minimalValidBridgeConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("Validate failed unexpectedly: %v", err)
	}
	assertDirectHoldFencingWarning(t, warnings)
}

// TestValidateWithWarnings_DefaultModeEmitsWarning verifies that an empty
// delivery_mode (defaults to direct_hold) also emits the fencing warning.
func TestValidateWithWarnings_DefaultModeEmitsWarning(t *testing.T) {
	cfg := minimalValidBridgeConfig()
	cfg.Routes[0].DeliveryMode = ""

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("Validate failed unexpectedly: %v", err)
	}
	assertDirectHoldFencingWarning(t, warnings)
}

// TestValidateWithWarnings_InvalidConfigReturnsWarningsAndError verifies that
// an invalid config with direct_hold returns both validation errors and the
// fencing warning.
func TestValidateWithWarnings_InvalidConfigReturnsWarningsAndError(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Routes: []ports.RouteDef{
			{ID: "route1", DeliveryMode: "direct_hold", Bindings: []string{"b1"}},
		},
	}

	warnings, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for config with no receivers")
	}

	hasWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "direct_hold") && strings.Contains(w, "fencing") {
			hasWarning = true
			break
		}
	}
	if !hasWarning {
		t.Fatalf("expected direct_hold fencing warning alongside error, got warnings: %v", warnings)
	}
}

// TestValidateWithWarnings_SharedOutboxNoWarning verifies that shared_outbox
// routes do not trigger the direct_hold fencing warning.
func TestValidateWithWarnings_SharedOutboxNoWarning(t *testing.T) {
	cfg := minimalValidBridgeConfig()
	cfg.Routes[0].DeliveryMode = "shared_outbox"
	cfg.Stores.Outbox = &ports.StoreConfig{Type: "memory"}

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("Validate failed unexpectedly: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "direct_hold") && strings.Contains(w, "fencing") {
			t.Fatalf("unexpected direct_hold fencing warning: %q", w)
		}
	}
}
