package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// TestToRoutePolicy_FieldMapping validates that all RouteDef fields are mapped
// to the corresponding RoutePolicy fields.
func TestToRoutePolicy_FieldMapping(t *testing.T) {
	rd := ports.RouteDef{
		DeliveryMode: "shared_outbox",
		DispatchMode: "fan_out",
		Policy: ports.PolicyDef{
			MaxInFlight:        50,
			MaxReplayAttempts:  3,
			MaxOutboxDepth:     500,
			AckAfter:           "outbox_persist",
			OnExpired:          "drop",
			OnPermanentFailure: "drop",
		},
	}

	p := toRoutePolicy(rd)

	if p.DeliveryMode != routing.DeliverySharedOutbox {
		t.Fatalf("DeliveryMode: got %q, want %q", p.DeliveryMode, routing.DeliverySharedOutbox)
	}
	if p.DispatchMode != routing.DispatchFanOut {
		t.Fatalf("DispatchMode: got %q, want %q", p.DispatchMode, routing.DispatchFanOut)
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
	if p.AckAfter != routing.AckAfterOutboxPersist {
		t.Fatalf("AckAfter: got %q, want %q", p.AckAfter, routing.AckAfterOutboxPersist)
	}
	if p.OnExpired != routing.ExpiredDrop {
		t.Fatalf("OnExpired: got %q, want %q", p.OnExpired, routing.ExpiredDrop)
	}
	if p.OnPermanentFailure != routing.FailureDrop {
		t.Fatalf("OnPermanentFailure: got %q, want %q", p.OnPermanentFailure, routing.FailureDrop)
	}
}

// TestToRoutePolicy_BackoffDurations validates that backoff duration strings
// are parsed correctly into time.Duration values.
func TestToRoutePolicy_BackoffDurations(t *testing.T) {
	rd := ports.RouteDef{
		Policy: ports.PolicyDef{
			Backoff: ports.BackoffDef{
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

// TestToRoutePolicy_BackoffJitter validates that BackoffDef.Jitter maps
// onto BackoffPolicy.JitterFactor (finding #27 config round-trip).
func TestToRoutePolicy_BackoffJitter(t *testing.T) {
	rd := ports.RouteDef{
		Policy: ports.PolicyDef{
			Backoff: ports.BackoffDef{
				InitialInterval: "500ms",
				Multiplier:      2.0,
				Jitter:          0.3,
			},
		},
	}

	p, err := toRoutePolicyE(rd)
	if err != nil {
		t.Fatalf("toRoutePolicyE: %v", err)
	}
	if p.Backoff.JitterFactor != 0.3 {
		t.Fatalf("Backoff.JitterFactor: got %v, want 0.3", p.Backoff.JitterFactor)
	}
}

// TestToSessionConfig_FromRouteSessionDef validates that RouteSessionDef
// fields are mapped to the session.Config struct.
func TestToSessionConfig_FromRouteSessionDef(t *testing.T) {
	rs := &ports.RouteSessionDef{
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

// TestToSessionConfig_DerivesRenewIntervalFromTTL validates contract C3: when
// only lease_ttl is configured, RenewInterval is left ZERO so the session
// manager derives it from LeaseTTL downstream. Seeding it (the old bug via
// session.DefaultConfig, which pins RenewInterval=110s) suppressed the
// derive-from-TTL branch and kept a 110s renew cadence regardless of a much
// shorter TTL.
func TestToSessionConfig_DerivesRenewIntervalFromTTL(t *testing.T) {
	rs := &ports.RouteSessionDef{SessionID: "s1", LeaseTTL: "45s"}

	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}
	if sc.LeaseTTL != 45*time.Second {
		t.Fatalf("LeaseTTL: got %v, want 45s", sc.LeaseTTL)
	}
	if sc.RenewInterval != 0 {
		t.Fatalf("RenewInterval must be zero (derived from TTL downstream) when only lease_ttl is set; got %v", sc.RenewInterval)
	}
}

// TestToSessionConfig_LeaseRenewJitter validates that the lease_renew_jitter
// YAML field is plumbed into session.Config.RenewJitter (contract C3).
func TestToSessionConfig_LeaseRenewJitter(t *testing.T) {
	rs := &ports.RouteSessionDef{SessionID: "s1", RenewJitter: "5s"}

	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}
	if sc.RenewJitter != 5*time.Second {
		t.Fatalf("RenewJitter: got %v, want 5s", sc.RenewJitter)
	}
}

// TestToSessionConfig_InvalidJitter_ReturnsError validates that a malformed
// lease_renew_jitter duration surfaces a clear error naming the field.
func TestToSessionConfig_InvalidJitter_ReturnsError(t *testing.T) {
	rs := &ports.RouteSessionDef{SessionID: "s1", RenewJitter: "not-a-duration"}

	_, err := toSessionConfigE(rs)
	if err == nil {
		t.Fatal("expected error for invalid lease_renew_jitter")
	}
	if !strings.Contains(err.Error(), "lease_renew_jitter") {
		t.Fatalf("error should name the lease_renew_jitter field; got %v", err)
	}
}

// TestToRoutePolicy_SendTimeoutAndDepthCacheTTL validates that the new
// duration fields are parsed and mapped correctly.
func TestToRoutePolicy_SendTimeoutAndDepthCacheTTL(t *testing.T) {
	rd := ports.RouteDef{
		Policy: ports.PolicyDef{
			SendTimeout:   "5s",
			DepthCacheTTL: "200ms",
		},
	}
	p, err := toRoutePolicyE(rd)
	if err != nil {
		t.Fatal(err)
	}
	if p.SendTimeout != 5*time.Second {
		t.Fatalf("SendTimeout: got %v, want 5s", p.SendTimeout)
	}
	if p.DepthCacheTTL != 200*time.Millisecond {
		t.Fatalf("DepthCacheTTL: got %v, want 200ms", p.DepthCacheTTL)
	}
}

// TestToRoutePolicy_AllowFlags validates that AllowUnfenced and AllowRetryDrop
// are wired from config to domain.
func TestToRoutePolicy_AllowFlags(t *testing.T) {
	rd := ports.RouteDef{
		Policy: ports.PolicyDef{
			AllowUnfenced:  true,
			AllowRetryDrop: true,
		},
	}
	p := toRoutePolicy(rd)
	if !p.AllowUnfenced {
		t.Fatal("AllowUnfenced should be true")
	}
	if !p.AllowRetryDrop {
		t.Fatal("AllowRetryDrop should be true")
	}
}

// TestToRoutePolicy_InvalidSendTimeout validates that invalid send_timeout
// duration strings return an error.
func TestToRoutePolicy_InvalidSendTimeout(t *testing.T) {
	rd := ports.RouteDef{
		Policy: ports.PolicyDef{SendTimeout: "banana"},
	}
	_, err := toRoutePolicyE(rd)
	if err == nil {
		t.Fatal("expected error for invalid send_timeout")
	}
}

// TestToSessionConfig_DrainMaxFields validates that DrainMaxBatchSize and
// DrainMaxConcurrency are wired from config to runtime.
func TestToSessionConfig_DrainMaxFields(t *testing.T) {
	rs := &ports.RouteSessionDef{
		SessionID:           "s1",
		DrainMaxBatchSize:   200,
		DrainMaxConcurrency: 5,
	}
	sc := toSessionConfig(rs)
	if sc == nil {
		t.Fatal("expected non-nil SessionConfig")
	}
	if sc.DrainMaxBatchSize != 200 {
		t.Fatalf("DrainMaxBatchSize: got %d, want 200", sc.DrainMaxBatchSize)
	}
	if sc.DrainMaxConcurrency != 5 {
		t.Fatalf("DrainMaxConcurrency: got %d, want 5", sc.DrainMaxConcurrency)
	}
}

// TestToDrainStrategy_FixedPoll validates fixed_poll drain strategy construction.
// FixedPoll applies ±25% jitter, so we check within tolerance.
func TestToDrainStrategy_FixedPoll(t *testing.T) {
	rs := &ports.RouteSessionDef{
		SessionID:     "s1",
		DrainInterval: "5s",
	}
	strategy := toDrainStrategy(rs)
	interval := strategy.NextInterval(0)
	lo := time.Duration(float64(5*time.Second) * 0.75)
	hi := time.Duration(float64(5*time.Second) * 1.25)
	if interval < lo || interval > hi {
		t.Fatalf("expected 5s ±25%% interval, got %v", interval)
	}
}

// TestToDrainStrategy_AdaptiveBackoff validates adaptive_backoff drain strategy
// construction from a DrainStrategyDef.
func TestToDrainStrategy_AdaptiveBackoff(t *testing.T) {
	rs := &ports.RouteSessionDef{
		SessionID: "s1",
		DrainStrategy: &ports.DrainStrategyDef{
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
