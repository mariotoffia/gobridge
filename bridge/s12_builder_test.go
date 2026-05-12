package bridge

import (
	"testing"
	"time"

	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// rawOutboxOptions wraps a map[string]any as a ports.RawConfig using
// the canonical cfgparser.NewRawConfig implementation so tests
// exercise the same decode path as production.
func rawOutboxOptions(m map[string]any) ports.RawConfig {
	return cfgparser.NewRawConfig(m)
}

func computeStaleClaimBuffer(maxStepDownGrace time.Duration) time.Duration {
	buf := 2 * maxStepDownGrace
	if buf < 15*time.Second {
		buf = 15 * time.Second
	}
	return buf
}

// TestOutboxRuntimeOptions_DefaultDerivation validates the default
// derivation: one route with default StepDownGrace (15s) produces
// stale_claim_duration = 30s.
func TestOutboxRuntimeOptions_DefaultDerivation(t *testing.T) {
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

	sc := &ports.StoreConfig{Type: "memory"}
	got, err := b.outboxRuntimeOptions(sc)
	if err != nil {
		t.Fatalf("outboxRuntimeOptions: %v", err)
	}
	defaultGrace := runtime.DefaultSessionConfig("", true).StepDownGrace
	want := defaultGrace + computeStaleClaimBuffer(defaultGrace)
	if got.StaleClaimDuration != want {
		t.Errorf("StaleClaimDuration: got %v, want %v", got.StaleClaimDuration, want)
	}
}

// TestOutboxRuntimeOptions_MaxAcrossRoutes validates that the
// derivation takes the maximum StepDownGrace across all routes.
func TestOutboxRuntimeOptions_MaxAcrossRoutes(t *testing.T) {
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

	sc := &ports.StoreConfig{Type: "memory"}
	got, err := b.outboxRuntimeOptions(sc)
	if err != nil {
		t.Fatalf("outboxRuntimeOptions: %v", err)
	}
	wantMax := 45*time.Second + computeStaleClaimBuffer(45*time.Second)
	if got.StaleClaimDuration != wantMax {
		t.Errorf("StaleClaimDuration: got %v, want %v", got.StaleClaimDuration, wantMax)
	}
}

// TestOutboxRuntimeOptions_ExplicitOverride validates that an
// explicit stale_claim_duration in the outbox raw options is
// honored verbatim (no derivation).
func TestOutboxRuntimeOptions_ExplicitOverride(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{{
			ID: "r1",
			Session: &ports.RouteSessionDef{
				SessionID: "s1",
				SenderID:  "tx1",
			},
		}},
	})

	sc := &ports.StoreConfig{Type: "memory"}
	sc.SetDecoded(nil, rawOutboxOptions(map[string]any{"stale_claim_duration": "2m"}))

	got, err := b.outboxRuntimeOptions(sc)
	if err != nil {
		t.Fatalf("outboxRuntimeOptions: %v", err)
	}
	if got.StaleClaimDuration != 2*time.Minute {
		t.Errorf("expected explicit override 2m preserved, got %v", got.StaleClaimDuration)
	}
}

// TestOutboxRuntimeOptions_NoRouteSession validates derivation when
// all routes have nil Session blocks.
func TestOutboxRuntimeOptions_NoRouteSession(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{
			{ID: "r1"},
			{ID: "r2"},
		},
	})

	sc := &ports.StoreConfig{Type: "memory"}
	got, err := b.outboxRuntimeOptions(sc)
	if err != nil {
		t.Fatalf("outboxRuntimeOptions: %v", err)
	}
	defaultGrace2 := runtime.DefaultSessionConfig("", true).StepDownGrace
	want2 := defaultGrace2 + computeStaleClaimBuffer(defaultGrace2)
	if got.StaleClaimDuration != want2 {
		t.Errorf("StaleClaimDuration: got %v, want %v", got.StaleClaimDuration, want2)
	}
}

// TestOutboxRuntimeOptions_InvalidExplicitOverride validates that an
// unparseable stale_claim_duration string returns an error.
func TestOutboxRuntimeOptions_InvalidExplicitOverride(t *testing.T) {
	b := newTestBuilder(&ports.BridgeConfig{
		Routes: []ports.RouteDef{{
			ID:      "r1",
			Session: &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
		}},
	})

	sc := &ports.StoreConfig{Type: "memory"}
	sc.SetDecoded(nil, rawOutboxOptions(map[string]any{"stale_claim_duration": "not-a-duration"}))

	if _, err := b.outboxRuntimeOptions(sc); err == nil {
		t.Fatal("expected error for invalid duration string")
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
