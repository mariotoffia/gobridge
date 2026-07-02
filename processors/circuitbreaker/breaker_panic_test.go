package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestBreaker_PanicInNext_LeavesHalfOpenProbeStuck validates that when
// afterRequest is skipped after a half-open probe (simulating raw breaker
// usage without the Processor's defer guard), the halfOpenInFlight counter
// stays elevated, blocking subsequent probes.
func TestBreaker_PanicInNext_LeavesHalfOpenProbeStuck(t *testing.T) {
	cfg := cb.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Millisecond,
	}
	fake := clocktest.New()
	b := cb.NewBreaker("panic-test", cfg, nil, cb.WithBreakerClock(fake))

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("beforeRequest in closed state: %v", err)
	}
	b.AfterRequest(errTest)

	m := b.GetMetrics()
	if m.State != "open" {
		t.Fatalf("expected open after 1 failure (threshold=1), got %s", m.State)
	}

	fake.Advance(cfg.ResetTimeout + 5*time.Millisecond)

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("expected half-open probe to be admitted: %v", err)
	}

	// Simulate panic: intentionally skip afterRequest. This mimics what
	// happens when next() panics in Process() — afterRequest is never
	// called because it is not deferred.

	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("halfOpenInFlight = %d, want 1 (stuck probe)", got)
	}

	err := b.BeforeRequest()
	if err == nil {
		t.Fatal("expected rejection due to stuck half-open probe, got nil")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// TestProcessor_PanicInNext_RecoversProperly validates that when next() panics
// inside Process(), the deferred afterRequest is still called so the breaker
// transitions correctly (panic counts as a failure).
func TestProcessor_PanicInNext_RecoversProperly(t *testing.T) {
	cfg := cb.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Millisecond,
	}
	fake := clocktest.New()
	p := New("panic-fix", cfg, WithClock(fake))
	ctx := context.Background()
	env := envelope("test", nil)

	err := p.Process(ctx, env, nextErr(errors.New("boom")))
	if err == nil {
		t.Fatal("expected error from failing next")
	}

	fake.Advance(cfg.ResetTimeout + 5*time.Millisecond)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic from next")
			}
		}()
		_ = p.Process(ctx, env, func(_ context.Context, _ *messaging.Envelope) error {
			panic("simulated downstream panic")
		})
	}()

	m := p.Metrics()["global"]
	if m.State != "open" {
		t.Fatalf("expected open (panic counted as failure), got %s", m.State)
	}
}

// TestState_String_UnknownValue validates that State.String() returns "unknown"
// for undefined state values.
func TestState_String_UnknownValue(t *testing.T) {
	tests := []struct {
		state cb.State
		want  string
	}{
		{cb.State(-1), "unknown"},
		{cb.State(3), "unknown"},
		{cb.State(99), "unknown"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("State(%d)", int(tc.state)), func(t *testing.T) {
			if got := tc.state.String(); got != tc.want {
				t.Errorf("State(%d).String() = %q, want %q", int(tc.state), got, tc.want)
			}
		})
	}
}

// TestHalfOpen_MaxProbesDefaultsToOne validates that HalfOpenMaxProbes of 0
// or negative defaults to 1 via newBreaker's normalization.
func TestHalfOpen_MaxProbesDefaultsToOne(t *testing.T) {
	tests := []struct {
		name  string
		value int
	}{
		{"zero defaults to 1", 0},
		{"negative defaults to 1", -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cb.Config{
				FailureThreshold:  3,
				SuccessThreshold:  2,
				ResetTimeout:      50 * time.Millisecond,
				HalfOpenMaxProbes: tc.value,
			}
			b := cb.NewBreaker("defaults", cfg, nil)
			if b.InternalConfig().HalfOpenMaxProbes != 1 {
				t.Errorf("HalfOpenMaxProbes = %d, want 1", b.InternalConfig().HalfOpenMaxProbes)
			}
		})
	}
}
