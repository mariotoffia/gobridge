package domain_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// ═══════════════════════════════════════════════════════════════════
// Route & Envelope Edge Case Audit Tests
//
// Validates edge cases identified by GO-006, SEC-004, QA-007, QA-008:
//   - DefaultBackoffPolicy mutability affects WithDefaults
//   - OutboxPartitionKey("", "") edge case
//   - RoutePolicy.WithDefaults completeness
// ═══════════════════════════════════════════════════════════════════

// TestDefaultBackoffPolicy_MutableGlobal validates that mutating the
// package-level DefaultBackoffPolicy affects subsequent WithDefaults calls.
// This documents the known risk (GO-006, SEC-004).
//
// ═══════════════════════════════════════════════════════════════════
// DefaultBackoffPolicy is a mutable `var`. Mutation changes all
// subsequent WithDefaults() calls globally. After the fix to use
// NewDefaultBackoffPolicy(), mutating the global no longer affects
// WithDefaults() output.
func TestDefaultBackoffPolicy_MutableGlobalDoesNotAffectWithDefaults(t *testing.T) {
	orig := domain.DefaultBackoffPolicy

	defer func() {
		domain.DefaultBackoffPolicy = orig
	}()

	domain.DefaultBackoffPolicy.Multiplier = 99.0

	p := domain.RoutePolicy{}.WithDefaults()
	expected := domain.NewDefaultBackoffPolicy().Multiplier
	if p.Backoff.Multiplier != expected {
		t.Fatalf("expected immutable default multiplier %f, got %f", expected, p.Backoff.Multiplier)
	}
}

// TestOutboxPartitionKey_BothEmpty validates the edge case where both
// sessionID and bindingID are empty strings, producing "BINDING#".
func TestOutboxPartitionKey_BothEmpty(t *testing.T) {
	key := persistence.OutboxPartitionKey("", "")
	if key != "BINDING#" {
		t.Fatalf("expected BINDING#, got %q", key)
	}
}

// TestOutboxPartitionKey_SessionTakesPrecedence validates that when
// both are non-empty, sessionID takes precedence.
func TestOutboxPartitionKey_SessionTakesPrecedence(t *testing.T) {
	key := persistence.OutboxPartitionKey("sess-1", "bind-1")
	if key != "SESSION#sess-1" {
		t.Fatalf("expected SESSION#sess-1, got %q", key)
	}
}

// TestRoutePolicy_WithDefaults_AllFieldsSet validates that WithDefaults
// does not overwrite explicitly set values.
func TestRoutePolicy_WithDefaults_AllFieldsSet(t *testing.T) {
	p := domain.RoutePolicy{
		MaxInFlight:        50,
		MaxReplayAttempts:  3,
		MaxOutboxDepth:     500,
		OnExpired:          domain.ExpiredDrop,
		OnPermanentFailure: domain.FailureDrop,
		DispatchMode:       domain.DispatchFanOut,
		DeliveryMode:       domain.DeliverySharedOutbox,
		AckAfter:           domain.AckAfterOutboxPersist,
		SendTimeout:        10 * time.Second,
		DepthCacheTTL:      5 * time.Second,
		Backoff: domain.BackoffPolicy{
			InitialInterval: 2 * time.Second,
			MaxInterval:     60 * time.Second,
			Multiplier:      3.0,
		},
	}

	result := p.WithDefaults()

	if result.MaxInFlight != 50 {
		t.Fatalf("MaxInFlight overwritten: got %d", result.MaxInFlight)
	}
	if result.MaxReplayAttempts != 3 {
		t.Fatalf("MaxReplayAttempts overwritten: got %d", result.MaxReplayAttempts)
	}
	if result.OnExpired != domain.ExpiredDrop {
		t.Fatalf("OnExpired overwritten: got %s", result.OnExpired)
	}
	if result.OnPermanentFailure != domain.FailureDrop {
		t.Fatalf("OnPermanentFailure overwritten: got %s", result.OnPermanentFailure)
	}
	if result.Backoff.Multiplier != 3.0 {
		t.Fatalf("Backoff.Multiplier overwritten: got %f", result.Backoff.Multiplier)
	}
	if result.SendTimeout != 10*time.Second {
		t.Fatalf("SendTimeout overwritten: got %v", result.SendTimeout)
	}
}

// TestRoutePolicy_WithDefaults_ZeroValue validates that a zero-value
// RoutePolicy gets all defaults applied.
func TestRoutePolicy_WithDefaults_ZeroValue(t *testing.T) {
	p := domain.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != domain.DefaultMaxInFlight {
		t.Fatalf("expected MaxInFlight=%d, got %d", domain.DefaultMaxInFlight, p.MaxInFlight)
	}
	if p.MaxReplayAttempts != domain.DefaultMaxReplayAttempts {
		t.Fatalf("expected MaxReplayAttempts=%d, got %d", domain.DefaultMaxReplayAttempts, p.MaxReplayAttempts)
	}
	if p.SendTimeout != domain.DefaultSendTimeout {
		t.Fatalf("expected SendTimeout=%v, got %v", domain.DefaultSendTimeout, p.SendTimeout)
	}
	if p.OnExpired != domain.ExpiredDLQ {
		t.Fatalf("expected OnExpired=dlq, got %s", p.OnExpired)
	}
	if p.OnPermanentFailure != domain.FailureDLQ {
		t.Fatalf("expected OnPermanentFailure=dlq, got %s", p.OnPermanentFailure)
	}
}
