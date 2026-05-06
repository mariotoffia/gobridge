package routing_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// TestRoutePolicy_WithDefaults verifies WithDefaults applies package defaults to all zero-valued RoutePolicy fields.
func TestRoutePolicy_WithDefaults(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != routing.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight: got %d, want %d", p.MaxInFlight, routing.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts: got %d, want %d", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
	}
	if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth: got %d, want %d", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
	}
	if p.Backoff.InitialInterval != 1*time.Second {
		t.Fatalf("Backoff.InitialInterval: got %v, want 1s", p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval != 30*time.Second {
		t.Fatalf("Backoff.MaxInterval: got %v, want 30s", p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier != 2.0 {
		t.Fatalf("Backoff.Multiplier: got %v, want 2.0", p.Backoff.Multiplier)
	}
	if p.OnExpired != routing.ExpiredDLQ {
		t.Fatalf("OnExpired: got %s, want %s", p.OnExpired, routing.ExpiredDLQ)
	}
	if p.OnPermanentFailure != routing.FailureDLQ {
		t.Fatalf("OnPermanentFailure: got %s, want %s", p.OnPermanentFailure, routing.FailureDLQ)
	}
	if p.DispatchMode != routing.DispatchSingle {
		t.Fatalf("DispatchMode: got %s, want %s", p.DispatchMode, routing.DispatchSingle)
	}
	if p.AckAfter != routing.AckAfterTargetAccept {
		t.Fatalf("AckAfter: got %s, want %s", p.AckAfter, routing.AckAfterTargetAccept)
	}
}

// TestRoutePolicy_WithDefaults_PreservesExplicit verifies WithDefaults does not overwrite explicitly set RoutePolicy and BackoffPolicy fields.
func TestRoutePolicy_WithDefaults_PreservesExplicit(t *testing.T) {
	p := routing.RoutePolicy{
		MaxInFlight:        50,
		MaxReplayAttempts:  10,
		OnExpired:          routing.ExpiredDrop,
		OnPermanentFailure: routing.FailureDrop,
		DispatchMode:       routing.DispatchFanOut,
		AckAfter:           routing.AckAfterOutboxPersist,
		Backoff: routing.BackoffPolicy{
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     10 * time.Second,
			Multiplier:      1.5,
		},
	}.WithDefaults()

	if p.MaxInFlight != 50 {
		t.Fatalf("explicit MaxInFlight should be preserved, got %d", p.MaxInFlight)
	}
	if p.MaxReplayAttempts != 10 {
		t.Fatalf("explicit MaxReplayAttempts should be preserved, got %d", p.MaxReplayAttempts)
	}
	if p.OnExpired != routing.ExpiredDrop {
		t.Fatal("explicit OnExpired should be preserved")
	}
	if p.OnPermanentFailure != routing.FailureDrop {
		t.Fatal("explicit OnPermanentFailure should be preserved")
	}
	if p.DispatchMode != routing.DispatchFanOut {
		t.Fatal("explicit DispatchMode should be preserved")
	}
	if p.AckAfter != routing.AckAfterOutboxPersist {
		t.Fatal("explicit AckAfter should be preserved")
	}
	if p.Backoff.InitialInterval != 500*time.Millisecond {
		t.Fatal("explicit Backoff.InitialInterval should be preserved")
	}
	if p.Backoff.Multiplier != 1.5 {
		t.Fatal("explicit Backoff.Multiplier should be preserved")
	}
}

// TestDefaultBackoffPolicy_Values validates the package-level DefaultBackoffPolicy variable.
func TestDefaultBackoffPolicy_Values(t *testing.T) {
	bp := routing.DefaultBackoffPolicy
	if bp.InitialInterval != 1*time.Second {
		t.Fatalf("InitialInterval: got %v, want 1s", bp.InitialInterval)
	}
	if bp.MaxInterval != 30*time.Second {
		t.Fatalf("MaxInterval: got %v, want 30s", bp.MaxInterval)
	}
	if bp.Multiplier != 2.0 {
		t.Fatalf("Multiplier: got %v, want 2.0", bp.Multiplier)
	}
}

// TestDeliveryMode_Constants validates that DeliveryMode enum values are distinct and non-empty.
func TestDeliveryMode_Constants(t *testing.T) {
	modes := []routing.DeliveryMode{
		routing.DeliveryDirectHold,
		routing.DeliverySharedOutbox,
	}
	seen := make(map[routing.DeliveryMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("DeliveryMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate DeliveryMode: %q", m)
		}
		seen[m] = true
	}
}

// TestDispatchMode_Constants validates that DispatchMode enum values are distinct and non-empty.
func TestDispatchMode_Constants(t *testing.T) {
	modes := []routing.DispatchMode{
		routing.DispatchSingle,
		routing.DispatchFanOut,
	}
	seen := make(map[routing.DispatchMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("DispatchMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate DispatchMode: %q", m)
		}
		seen[m] = true
	}
}

// TestSessionMode_Constants validates that SessionMode enum values are distinct and non-empty.
func TestSessionMode_Constants(t *testing.T) {
	modes := []connectivity.SessionMode{
		connectivity.SessionEphemeral,
		connectivity.SessionPersistent,
		connectivity.SessionExclusive,
	}
	seen := make(map[connectivity.SessionMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("SessionMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate SessionMode: %q", m)
		}
		seen[m] = true
	}
}

// TestAckBoundary_Constants validates that AckBoundary enum values are distinct and non-empty.
func TestAckBoundary_Constants(t *testing.T) {
	values := []routing.AckBoundary{
		routing.AckAfterTargetAccept,
		routing.AckAfterOutboxPersist,
	}
	seen := make(map[routing.AckBoundary]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("AckBoundary constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate AckBoundary: %q", v)
		}
		seen[v] = true
	}
}

// TestExpiredAction_Constants validates that ExpiredAction enum values are distinct and non-empty.
func TestExpiredAction_Constants(t *testing.T) {
	values := []routing.ExpiredAction{
		routing.ExpiredDrop,
		routing.ExpiredDLQ,
	}
	seen := make(map[routing.ExpiredAction]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("ExpiredAction constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate ExpiredAction: %q", v)
		}
		seen[v] = true
	}
}

// TestFailureAction_Constants validates that FailureAction enum values are distinct and non-empty.
func TestFailureAction_Constants(t *testing.T) {
	values := []routing.FailureAction{
		routing.FailureDLQ,
		routing.FailureDrop,
	}
	seen := make(map[routing.FailureAction]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("FailureAction constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate FailureAction: %q", v)
		}
		seen[v] = true
	}
}

// TestRoutePolicy_WithDefaults_NegativeValues verifies that negative values for
// MaxInFlight, MaxReplayAttempts, and MaxOutboxDepth are clamped to defaults.
func TestRoutePolicy_WithDefaults_NegativeValues(t *testing.T) {
	tests := []struct {
		name  string
		build func() routing.RoutePolicy
		check func(t *testing.T, p routing.RoutePolicy)
	}{
		{
			name: "negative MaxInFlight clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxInFlight: -1}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxInFlight != routing.DefaultMaxInFlight {
					t.Fatalf("MaxInFlight = %d, want %d (negative clamped)", p.MaxInFlight, routing.DefaultMaxInFlight)
				}
			},
		},
		{
			name: "negative MaxReplayAttempts clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxReplayAttempts: -5}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
					t.Fatalf("MaxReplayAttempts = %d, want %d (negative clamped)", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
				}
			},
		},
		{
			name: "negative MaxOutboxDepth clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxOutboxDepth: -100}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
					t.Fatalf("MaxOutboxDepth = %d, want %d (negative clamped)", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, tt.build())
		})
	}
}

// TestRoutePolicy_WithDefaults_AllDefaults verifies that a completely zero-valued
// RoutePolicy receives every default, including SendTimeout and DepthCacheTTL.
func TestRoutePolicy_WithDefaults_AllDefaults(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != routing.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want %d", p.MaxInFlight, routing.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts = %d, want %d", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
	}
	if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth = %d, want %d", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
	}
	if p.Backoff.InitialInterval != routing.DefaultBackoffPolicy.InitialInterval {
		t.Fatalf("Backoff.InitialInterval = %v, want %v", p.Backoff.InitialInterval, routing.DefaultBackoffPolicy.InitialInterval)
	}
	if p.Backoff.MaxInterval != routing.DefaultBackoffPolicy.MaxInterval {
		t.Fatalf("Backoff.MaxInterval = %v, want %v", p.Backoff.MaxInterval, routing.DefaultBackoffPolicy.MaxInterval)
	}
	if p.Backoff.Multiplier != routing.DefaultBackoffPolicy.Multiplier {
		t.Fatalf("Backoff.Multiplier = %v, want %v", p.Backoff.Multiplier, routing.DefaultBackoffPolicy.Multiplier)
	}
	if p.OnExpired != routing.ExpiredDLQ {
		t.Fatalf("OnExpired = %s, want %s", p.OnExpired, routing.ExpiredDLQ)
	}
	if p.OnPermanentFailure != routing.FailureDLQ {
		t.Fatalf("OnPermanentFailure = %s, want %s", p.OnPermanentFailure, routing.FailureDLQ)
	}
	if p.DispatchMode != routing.DispatchSingle {
		t.Fatalf("DispatchMode = %s, want %s", p.DispatchMode, routing.DispatchSingle)
	}
	if p.DeliveryMode != routing.DeliveryDirectHold {
		t.Fatalf("DeliveryMode = %s, want %s", p.DeliveryMode, routing.DeliveryDirectHold)
	}
	if p.AckAfter != routing.AckAfterTargetAccept {
		t.Fatalf("AckAfter = %s, want %s", p.AckAfter, routing.AckAfterTargetAccept)
	}
	if p.SendTimeout != routing.DefaultSendTimeout {
		t.Fatalf("SendTimeout = %v, want %v", p.SendTimeout, routing.DefaultSendTimeout)
	}
	if p.DepthCacheTTL != routing.DefaultDepthCacheTTL {
		t.Fatalf("DepthCacheTTL = %v, want %v", p.DepthCacheTTL, routing.DefaultDepthCacheTTL)
	}
}

// TestDefaultBackoffPolicy_MutationDoesNotAffectWithDefaults verifies that
// mutating the global DefaultBackoffPolicy var does not affect WithDefaults().
// WithDefaults() uses NewDefaultBackoffPolicy() for immutable defaults.
func TestDefaultBackoffPolicy_MutationDoesNotAffectWithDefaults(t *testing.T) {
	saved := routing.DefaultBackoffPolicy
	t.Cleanup(func() { routing.DefaultBackoffPolicy = saved })

	routing.DefaultBackoffPolicy = routing.BackoffPolicy{
		InitialInterval: 999 * time.Millisecond,
		MaxInterval:     999 * time.Millisecond,
		Multiplier:      99.0,
	}

	p := routing.RoutePolicy{}.WithDefaults()
	defaults := routing.NewDefaultBackoffPolicy()
	if p.Backoff.InitialInterval != defaults.InitialInterval {
		t.Fatalf("expected immutable InitialInterval %v, got %v", defaults.InitialInterval, p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval != defaults.MaxInterval {
		t.Fatalf("expected immutable MaxInterval %v, got %v", defaults.MaxInterval, p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier != defaults.Multiplier {
		t.Fatalf("expected immutable Multiplier %v, got %v", defaults.Multiplier, p.Backoff.Multiplier)
	}
}
