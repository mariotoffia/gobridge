package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// S12 Builder Tests — injectStaleClaimDuration
//
// Validates that the builder derives stale_claim_duration from the
// maximum StepDownGrace across all route sessions and injects it
// into the outbox store config options.
//
//   max(StepDownGrace) + 15s = stale_claim_duration
//
// ═══════════════════════════════════════════════════════════════════════

// TestInjectStaleClaimDuration_DefaultDerivation validates the default
// derivation: one route with default StepDownGrace (15s) produces
// stale_claim_duration = 30s.
func TestInjectStaleClaimDuration_DefaultDerivation(t *testing.T) {
	b := newTestBuilder(&config.BridgeConfig{
		Stores: config.StoresConfig{
			Outbox: &config.StoreConfig{Type: "memory"},
		},
		Routes: []config.RouteDef{{
			ID: "r1",
			Session: &config.RouteSessionDef{
				SessionID: "s1",
				SenderID:  "tx1",
			},
		}},
	})

	sc := &config.StoreConfig{Type: "memory"}
	b.injectStaleClaimDuration(sc)

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	want := runtime.DefaultSessionConfig("", true).StepDownGrace + staleClaimBuffer
	if got != want {
		t.Errorf("stale_claim_duration: got %v, want %v", got, want)
	}
}

// TestInjectStaleClaimDuration_MaxAcrossRoutes validates that the
// derivation takes the maximum StepDownGrace across all routes.
func TestInjectStaleClaimDuration_MaxAcrossRoutes(t *testing.T) {
	b := newTestBuilder(&config.BridgeConfig{
		Routes: []config.RouteDef{
			{
				ID: "r1",
				Session: &config.RouteSessionDef{
					SessionID:     "s1",
					SenderID:      "tx1",
					StepDownGrace: "20s",
				},
			},
			{
				ID: "r2",
				Session: &config.RouteSessionDef{
					SessionID:     "s2",
					SenderID:      "tx2",
					StepDownGrace: "45s",
				},
			},
		},
	})

	sc := &config.StoreConfig{Type: "memory"}
	b.injectStaleClaimDuration(sc)

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	if got != 45*time.Second+staleClaimBuffer {
		t.Errorf("stale_claim_duration: got %v, want %v", got, 45*time.Second+staleClaimBuffer)
	}
}

// TestInjectStaleClaimDuration_ExplicitSkipped validates that an
// explicitly set stale_claim_duration is not overwritten.
func TestInjectStaleClaimDuration_ExplicitSkipped(t *testing.T) {
	b := newTestBuilder(&config.BridgeConfig{
		Routes: []config.RouteDef{{
			ID: "r1",
			Session: &config.RouteSessionDef{
				SessionID: "s1",
				SenderID:  "tx1",
			},
		}},
	})

	sc := &config.StoreConfig{
		Type:    "memory",
		Options: map[string]any{"stale_claim_duration": "2m"},
	}
	b.injectStaleClaimDuration(sc)

	got := sc.Options["stale_claim_duration"]
	if got != "2m" {
		t.Errorf("expected explicit value preserved, got %v", got)
	}
}

// TestInjectStaleClaimDuration_NoRouteSession validates derivation
// when all routes have nil Session blocks.
func TestInjectStaleClaimDuration_NoRouteSession(t *testing.T) {
	b := newTestBuilder(&config.BridgeConfig{
		Routes: []config.RouteDef{
			{ID: "r1"},
			{ID: "r2"},
		},
	})

	sc := &config.StoreConfig{Type: "memory"}
	b.injectStaleClaimDuration(sc)

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	want := runtime.DefaultSessionConfig("", true).StepDownGrace + staleClaimBuffer
	if got != want {
		t.Errorf("stale_claim_duration: got %v, want %v", got, want)
	}
}

// TestInjectStaleClaimDuration_DoesNotMutateOriginalOptions validates
// that the method does not mutate the original Options map, allowing
// safe re-derivation on subsequent Build() calls.
func TestInjectStaleClaimDuration_DoesNotMutateOriginalOptions(t *testing.T) {
	original := map[string]any{"table_name": "my-outbox"}
	sc := &config.StoreConfig{
		Type:    "memory",
		Options: original,
	}

	b := newTestBuilder(&config.BridgeConfig{
		Routes: []config.RouteDef{{
			ID:      "r1",
			Session: &config.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
		}},
	})

	b.injectStaleClaimDuration(sc)

	if _, exists := original["stale_claim_duration"]; exists {
		t.Error("original options map was mutated — stale_claim_duration should not be in the original")
	}

	if _, exists := sc.Options["stale_claim_duration"]; !exists {
		t.Error("new options map should contain stale_claim_duration")
	}
}

// TestToSessionConfig_MaxRenewFails_And_StepDownGrace validates that
// toSessionConfig wires MaxRenewFails and StepDownGrace from YAML.
func TestToSessionConfig_MaxRenewFails_And_StepDownGrace(t *testing.T) {
	rs := &config.RouteSessionDef{
		SessionID:     "s1",
		SenderID:      "tx1",
		MaxRenewFails: 5,
		StepDownGrace: "30s",
	}
	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}
	if sc.MaxRenewFails != 5 {
		t.Errorf("MaxRenewFails: got %d, want 5", sc.MaxRenewFails)
	}
	if sc.StepDownGrace != 30*time.Second {
		t.Errorf("StepDownGrace: got %v, want 30s", sc.StepDownGrace)
	}
}

// TestToSessionConfig_DefaultsWhenOmitted validates that omitting
// MaxRenewFails and StepDownGrace uses DefaultSessionConfig values.
func TestToSessionConfig_DefaultsWhenOmitted(t *testing.T) {
	rs := &config.RouteSessionDef{
		SessionID: "s1",
		SenderID:  "tx1",
	}
	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}

	defaults := runtime.DefaultSessionConfig("s1", true)
	if sc.MaxRenewFails != defaults.MaxRenewFails {
		t.Errorf("MaxRenewFails: got %d, want %d", sc.MaxRenewFails, defaults.MaxRenewFails)
	}
	if sc.StepDownGrace != defaults.StepDownGrace {
		t.Errorf("StepDownGrace: got %v, want %v", sc.StepDownGrace, defaults.StepDownGrace)
	}
}

func newTestBuilder(cfg *config.BridgeConfig) *Builder {
	return NewBuilder(cfg)
}
