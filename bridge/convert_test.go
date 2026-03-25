package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
)

// TestToRoutePolicy_FieldMapping validates that all RouteDef fields are mapped
// to the corresponding RoutePolicy fields.
func TestToRoutePolicy_FieldMapping(t *testing.T) {
	rd := config.RouteDef{
		DeliveryMode: "shared_outbox",
		DispatchMode: "fan_out",
		Policy: config.PolicyDef{
			MaxInFlight:        50,
			MaxReplayAttempts:  3,
			MaxOutboxDepth:     500,
			AckAfter:           "outbox_persist",
			OnExpired:          "drop",
			OnPermanentFailure: "drop",
		},
	}

	p := toRoutePolicy(rd)

	if p.DeliveryMode != domain.DeliverySharedOutbox {
		t.Fatalf("DeliveryMode: got %q, want %q", p.DeliveryMode, domain.DeliverySharedOutbox)
	}
	if p.DispatchMode != domain.DispatchFanOut {
		t.Fatalf("DispatchMode: got %q, want %q", p.DispatchMode, domain.DispatchFanOut)
	}
	if p.MaxInFlight != 50 {
		t.Fatalf("MaxInFlight: got %d, want 50", p.MaxInFlight)
	}
	if p.MaxReplayAttempts != 3 {
		t.Fatalf("MaxReplayAttempts: got %d, want 3", p.MaxReplayAttempts)
	}
	if p.MaxOutboxDepth != 500 {
		t.Fatalf("MaxOutboxDepth: got %d, want 500", p.MaxOutboxDepth)
	}
	if p.AckAfter != domain.AckAfterOutboxPersist {
		t.Fatalf("AckAfter: got %q, want %q", p.AckAfter, domain.AckAfterOutboxPersist)
	}
	if p.OnExpired != domain.ExpiredDrop {
		t.Fatalf("OnExpired: got %q, want %q", p.OnExpired, domain.ExpiredDrop)
	}
	if p.OnPermanentFailure != domain.FailureDrop {
		t.Fatalf("OnPermanentFailure: got %q, want %q", p.OnPermanentFailure, domain.FailureDrop)
	}
}

// TestToRoutePolicy_BackoffDurations validates that backoff duration strings
// are parsed correctly into time.Duration values.
func TestToRoutePolicy_BackoffDurations(t *testing.T) {
	rd := config.RouteDef{
		Policy: config.PolicyDef{
			Backoff: config.BackoffDef{
				InitialInterval: "500ms",
				MaxInterval:     "10s",
				Multiplier:      1.5,
			},
		},
	}

	p := toRoutePolicy(rd)

	if p.Backoff.InitialInterval != 500*time.Millisecond {
		t.Fatalf("Backoff.InitialInterval: got %v, want 500ms", p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval != 10*time.Second {
		t.Fatalf("Backoff.MaxInterval: got %v, want 10s", p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier != 1.5 {
		t.Fatalf("Backoff.Multiplier: got %v, want 1.5", p.Backoff.Multiplier)
	}
}

// TestToSessionConfig_FromRouteSessionDef validates that RouteSessionDef
// fields are mapped to the runtime.SessionConfig struct.
func TestToSessionConfig_FromRouteSessionDef(t *testing.T) {
	rs := &config.RouteSessionDef{
		SessionID:         "mqtt-sess",
		LeaseTTL:          "60s",
		RenewInterval:     "20s",
		DrainBatchSize:    50,
		ConnectAfterLease: true,
	}

	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}
	if sc.LeaseTTL != 60*time.Second {
		t.Fatalf("LeaseTTL: got %v, want 60s", sc.LeaseTTL)
	}
	if sc.RenewInterval != 20*time.Second {
		t.Fatalf("RenewInterval: got %v, want 20s", sc.RenewInterval)
	}
	if sc.DrainBatchSize != 50 {
		t.Fatalf("DrainBatchSize: got %d, want 50", sc.DrainBatchSize)
	}
	if !sc.ConnectAfterLease {
		t.Fatal("ConnectAfterLease should be true")
	}
}

// TestToSessionConfig_NilReturnsNil validates that nil input returns nil.
func TestToSessionConfig_NilReturnsNil(t *testing.T) {
	sc := toSessionConfig(nil)
	if sc != nil {
		t.Fatal("expected nil for nil input")
	}
}

// TestToDrainStrategy_FixedPoll validates fixed_poll drain strategy construction.
func TestToDrainStrategy_FixedPoll(t *testing.T) {
	rs := &config.RouteSessionDef{
		SessionID:     "s1",
		DrainInterval: "5s",
	}
	strategy := toDrainStrategy(rs)
	interval := strategy.NextInterval(0)
	if interval != 5*time.Second {
		t.Fatalf("expected 5s interval, got %v", interval)
	}
}

// TestToDrainStrategy_AdaptiveBackoff validates adaptive_backoff drain strategy
// construction from a DrainStrategyDef.
func TestToDrainStrategy_AdaptiveBackoff(t *testing.T) {
	rs := &config.RouteSessionDef{
		SessionID: "s1",
		DrainStrategy: &config.DrainStrategyDef{
			Type:        "adaptive_backoff",
			MinInterval: "1s",
			MaxInterval: "30s",
			Multiplier:  2.0,
		},
	}
	strategy := toDrainStrategy(rs)
	interval := strategy.NextInterval(0)
	if interval < 1*time.Second || interval > 30*time.Second {
		t.Fatalf("expected interval between 1s and 30s, got %v", interval)
	}
}
