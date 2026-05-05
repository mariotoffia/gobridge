package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

func computeStaleClaimBuffer(maxStepDownGrace time.Duration) time.Duration {
	buf := 2 * maxStepDownGrace
	if buf < 15*time.Second {
		buf = 15 * time.Second
	}
	return buf
}

// TestInjectStaleClaimDuration_DefaultDerivation validates the default
// derivation: one route with default StepDownGrace (15s) produces
// stale_claim_duration = 30s.
func TestInjectStaleClaimDuration_DefaultDerivation(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Stores: ports.StoresConfig{
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Routes: []ports.RouteDef{{
			ID: "r1",
			Session: &ports.RouteSessionDef{
				SessionID: "s1",
				SenderID:  "tx1",
			},
		}},
	})

	sc := &ports.StoreSpec{Type: "memory"}
	if err := b.injectStaleClaimDuration(sc); err != nil {
		t.Fatalf("injectStaleClaimDuration: %v", err)
	}

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	defaultGrace := runtime.DefaultSessionConfig("", true).StepDownGrace
	want := defaultGrace + computeStaleClaimBuffer(defaultGrace)
	if got != want {
		t.Errorf("stale_claim_duration: got %v, want %v", got, want)
	}
}

// TestInjectStaleClaimDuration_MaxAcrossRoutes validates that the
// derivation takes the maximum StepDownGrace across all routes.
func TestInjectStaleClaimDuration_MaxAcrossRoutes(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{
			{
				ID: "r1",
				Session: &ports.RouteSessionDef{
					SessionID:     "s1",
					SenderID:      "tx1",
					StepDownGrace: "20s",
				},
			},
			{
				ID: "r2",
				Session: &ports.RouteSessionDef{
					SessionID:     "s2",
					SenderID:      "tx2",
					StepDownGrace: "45s",
				},
			},
		},
	})

	sc := &ports.StoreSpec{Type: "memory"}
	if err := b.injectStaleClaimDuration(sc); err != nil {
		t.Fatalf("injectStaleClaimDuration: %v", err)
	}

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	wantMax := 45*time.Second + computeStaleClaimBuffer(45*time.Second)
	if got != wantMax {
		t.Errorf("stale_claim_duration: got %v, want %v", got, wantMax)
	}
}

// TestInjectStaleClaimDuration_ExplicitSkipped validates that an
// explicitly set stale_claim_duration is not overwritten.
func TestInjectStaleClaimDuration_ExplicitSkipped(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{{
			ID: "r1",
			Session: &ports.RouteSessionDef{
				SessionID: "s1",
				SenderID:  "tx1",
			},
		}},
	})

	sc := &ports.StoreSpec{
		Type:    "memory",
		Options: map[string]any{"stale_claim_duration": "2m"},
	}
	if err := b.injectStaleClaimDuration(sc); err != nil {
		t.Fatalf("injectStaleClaimDuration: %v", err)
	}

	got := sc.Options["stale_claim_duration"]
	if got != "2m" {
		t.Errorf("expected explicit value preserved, got %v", got)
	}
}

// TestInjectStaleClaimDuration_NoRouteSession validates derivation
// when all routes have nil Session blocks.
func TestInjectStaleClaimDuration_NoRouteSession(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{
			{ID: "r1"},
			{ID: "r2"},
		},
	})

	sc := &ports.StoreSpec{Type: "memory"}
	if err := b.injectStaleClaimDuration(sc); err != nil {
		t.Fatalf("injectStaleClaimDuration: %v", err)
	}

	got, ok := sc.Options["stale_claim_duration"].(time.Duration)
	if !ok {
		t.Fatalf("expected time.Duration, got %T", sc.Options["stale_claim_duration"])
	}
	defaultGrace2 := runtime.DefaultSessionConfig("", true).StepDownGrace
	want2 := defaultGrace2 + computeStaleClaimBuffer(defaultGrace2)
	if got != want2 {
		t.Errorf("stale_claim_duration: got %v, want %v", got, want2)
	}
}

// TestInjectStaleClaimDuration_DoesNotMutateOriginalOptions validates
// that the method does not mutate the original Options map, allowing
// safe re-derivation on subsequent Build() calls.
func TestInjectStaleClaimDuration_DoesNotMutateOriginalOptions(t *testing.T) {
	original := map[string]any{"table_name": "my-outbox"}
	sc := &ports.StoreSpec{
		Type:    "memory",
		Options: original,
	}

	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{{
			ID:      "r1",
			Session: &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
		}},
	})

	if err := b.injectStaleClaimDuration(sc); err != nil {
		t.Fatalf("injectStaleClaimDuration: %v", err)
	}

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
	rs := &ports.RouteSessionDef{
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
	rs := &ports.RouteSessionDef{
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

func newTestBuilder(cfg *ports.BridgeConfig) *Builder {
	return NewBuilder(cfg)
}
