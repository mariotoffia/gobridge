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
