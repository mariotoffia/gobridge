package domain_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// TestRoutePolicy_WithDefaults verifies WithDefaults applies package defaults to all zero-valued RoutePolicy fields.
func TestRoutePolicy_WithDefaults(t *testing.T) {
	p := domain.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != domain.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight: got %d, want %d", p.MaxInFlight, domain.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != domain.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts: got %d, want %d", p.MaxReplayAttempts, domain.DefaultMaxReplayAttempts)
	}
	if p.MaxOutboxDepth != domain.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth: got %d, want %d", p.MaxOutboxDepth, domain.DefaultMaxOutboxDepth)
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
	if p.OnExpired != domain.ExpiredDLQ {
		t.Fatalf("OnExpired: got %s, want %s", p.OnExpired, domain.ExpiredDLQ)
	}
	if p.OnPermanentFailure != domain.FailureDLQ {
		t.Fatalf("OnPermanentFailure: got %s, want %s", p.OnPermanentFailure, domain.FailureDLQ)
	}
	if p.DispatchMode != domain.DispatchSingle {
		t.Fatalf("DispatchMode: got %s, want %s", p.DispatchMode, domain.DispatchSingle)
	}
	if p.AckAfter != domain.AckAfterTargetAccept {
		t.Fatalf("AckAfter: got %s, want %s", p.AckAfter, domain.AckAfterTargetAccept)
	}
}

// TestRoutePolicy_WithDefaults_PreservesExplicit verifies WithDefaults does not overwrite explicitly set RoutePolicy and BackoffPolicy fields.
func TestRoutePolicy_WithDefaults_PreservesExplicit(t *testing.T) {
	p := domain.RoutePolicy{
		MaxInFlight:        50,
		MaxReplayAttempts:  10,
		OnExpired:          domain.ExpiredDrop,
		OnPermanentFailure: domain.FailureDrop,
		DispatchMode:       domain.DispatchFanOut,
		AckAfter:           domain.AckAfterOutboxPersist,
		Backoff: domain.BackoffPolicy{
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
	if p.OnExpired != domain.ExpiredDrop {
		t.Fatal("explicit OnExpired should be preserved")
	}
	if p.OnPermanentFailure != domain.FailureDrop {
		t.Fatal("explicit OnPermanentFailure should be preserved")
	}
	if p.DispatchMode != domain.DispatchFanOut {
		t.Fatal("explicit DispatchMode should be preserved")
	}
	if p.AckAfter != domain.AckAfterOutboxPersist {
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
	bp := domain.DefaultBackoffPolicy
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
	modes := []domain.DeliveryMode{
		domain.DeliveryDirectHold,
		domain.DeliverySharedOutbox,
	}
	seen := make(map[domain.DeliveryMode]bool, len(modes))
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
	modes := []domain.DispatchMode{
		domain.DispatchSingle,
		domain.DispatchFanOut,
	}
	seen := make(map[domain.DispatchMode]bool, len(modes))
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
	modes := []domain.SessionMode{
		domain.SessionEphemeral,
		domain.SessionPersistent,
		domain.SessionExclusive,
	}
	seen := make(map[domain.SessionMode]bool, len(modes))
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
	values := []domain.AckBoundary{
		domain.AckAfterTargetAccept,
		domain.AckAfterOutboxPersist,
	}
	seen := make(map[domain.AckBoundary]bool, len(values))
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
	values := []domain.ExpiredAction{
		domain.ExpiredDrop,
		domain.ExpiredDLQ,
	}
	seen := make(map[domain.ExpiredAction]bool, len(values))
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
	values := []domain.FailureAction{
		domain.FailureDLQ,
		domain.FailureDrop,
	}
	seen := make(map[domain.FailureAction]bool, len(values))
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
		build func() domain.RoutePolicy
		check func(t *testing.T, p domain.RoutePolicy)
	}{
		{
			name: "negative MaxInFlight clamped to default",
			build: func() domain.RoutePolicy {
				return domain.RoutePolicy{MaxInFlight: -1}.WithDefaults()
			},
			check: func(t *testing.T, p domain.RoutePolicy) {
				if p.MaxInFlight != domain.DefaultMaxInFlight {
					t.Fatalf("MaxInFlight = %d, want %d (negative clamped)", p.MaxInFlight, domain.DefaultMaxInFlight)
				}
			},
		},
		{
			name: "negative MaxReplayAttempts clamped to default",
			build: func() domain.RoutePolicy {
				return domain.RoutePolicy{MaxReplayAttempts: -5}.WithDefaults()
			},
			check: func(t *testing.T, p domain.RoutePolicy) {
				if p.MaxReplayAttempts != domain.DefaultMaxReplayAttempts {
					t.Fatalf("MaxReplayAttempts = %d, want %d (negative clamped)", p.MaxReplayAttempts, domain.DefaultMaxReplayAttempts)
				}
			},
		},
		{
			name: "negative MaxOutboxDepth clamped to default",
			build: func() domain.RoutePolicy {
				return domain.RoutePolicy{MaxOutboxDepth: -100}.WithDefaults()
			},
			check: func(t *testing.T, p domain.RoutePolicy) {
				if p.MaxOutboxDepth != domain.DefaultMaxOutboxDepth {
					t.Fatalf("MaxOutboxDepth = %d, want %d (negative clamped)", p.MaxOutboxDepth, domain.DefaultMaxOutboxDepth)
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
	p := domain.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != domain.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want %d", p.MaxInFlight, domain.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != domain.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts = %d, want %d", p.MaxReplayAttempts, domain.DefaultMaxReplayAttempts)
	}
	if p.MaxOutboxDepth != domain.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth = %d, want %d", p.MaxOutboxDepth, domain.DefaultMaxOutboxDepth)
	}
	if p.Backoff.InitialInterval != domain.DefaultBackoffPolicy.InitialInterval {
		t.Fatalf("Backoff.InitialInterval = %v, want %v", p.Backoff.InitialInterval, domain.DefaultBackoffPolicy.InitialInterval)
	}
	if p.Backoff.MaxInterval != domain.DefaultBackoffPolicy.MaxInterval {
		t.Fatalf("Backoff.MaxInterval = %v, want %v", p.Backoff.MaxInterval, domain.DefaultBackoffPolicy.MaxInterval)
	}
	if p.Backoff.Multiplier != domain.DefaultBackoffPolicy.Multiplier {
		t.Fatalf("Backoff.Multiplier = %v, want %v", p.Backoff.Multiplier, domain.DefaultBackoffPolicy.Multiplier)
	}
	if p.OnExpired != domain.ExpiredDLQ {
		t.Fatalf("OnExpired = %s, want %s", p.OnExpired, domain.ExpiredDLQ)
	}
	if p.OnPermanentFailure != domain.FailureDLQ {
		t.Fatalf("OnPermanentFailure = %s, want %s", p.OnPermanentFailure, domain.FailureDLQ)
	}
	if p.DispatchMode != domain.DispatchSingle {
		t.Fatalf("DispatchMode = %s, want %s", p.DispatchMode, domain.DispatchSingle)
	}
	if p.DeliveryMode != domain.DeliveryDirectHold {
		t.Fatalf("DeliveryMode = %s, want %s", p.DeliveryMode, domain.DeliveryDirectHold)
	}
	if p.AckAfter != domain.AckAfterTargetAccept {
		t.Fatalf("AckAfter = %s, want %s", p.AckAfter, domain.AckAfterTargetAccept)
	}
	if p.SendTimeout != domain.DefaultSendTimeout {
		t.Fatalf("SendTimeout = %v, want %v", p.SendTimeout, domain.DefaultSendTimeout)
	}
	if p.DepthCacheTTL != domain.DefaultDepthCacheTTL {
		t.Fatalf("DepthCacheTTL = %v, want %v", p.DepthCacheTTL, domain.DefaultDepthCacheTTL)
	}
}

// TestDefaultBackoffPolicy_MutationDoesNotAffectWithDefaults verifies that
// mutating the global DefaultBackoffPolicy var does not affect WithDefaults().
// WithDefaults() uses NewDefaultBackoffPolicy() for immutable defaults.
func TestDefaultBackoffPolicy_MutationDoesNotAffectWithDefaults(t *testing.T) {
	saved := domain.DefaultBackoffPolicy
	t.Cleanup(func() { domain.DefaultBackoffPolicy = saved })

	domain.DefaultBackoffPolicy = domain.BackoffPolicy{
		InitialInterval: 999 * time.Millisecond,
		MaxInterval:     999 * time.Millisecond,
		Multiplier:      99.0,
	}

	p := domain.RoutePolicy{}.WithDefaults()
	defaults := domain.NewDefaultBackoffPolicy()
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
