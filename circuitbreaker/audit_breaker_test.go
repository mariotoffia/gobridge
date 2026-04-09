package circuitbreaker_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
)

// ═══════════════════════════════════════════════════════════════════
// Circuit Breaker Audit Tests
//
// Validates edge cases identified by QA-030, QA-032:
//   - State.String() for unknown state value
//   - Concurrent half-open probe limiting
//   - Config.WithDefaults zero-value handling
// ═══════════════════════════════════════════════════════════════════

// TestState_String_Unknown validates that an invalid State value
// returns "unknown".
func TestState_String_Unknown(t *testing.T) {
	invalid := circuitbreaker.State(99)
	if invalid.String() != "unknown" {
		t.Fatalf("expected 'unknown' for invalid state, got %q", invalid.String())
	}
}

// TestState_String_AllValid validates all valid state string representations.
func TestState_String_AllValid(t *testing.T) {
	tests := []struct {
		state circuitbreaker.State
		want  string
	}{
		{circuitbreaker.StateClosed, "closed"},
		{circuitbreaker.StateOpen, "open"},
		{circuitbreaker.StateHalfOpen, "half-open"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if tt.state.String() != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, tt.state.String())
			}
		})
	}
}

// TestBreaker_ConcurrentHalfOpenProbes validates that at most
// HalfOpenMaxProbes concurrent requests pass through in half-open state.
//
// ═══════════════════════════════════════════════════════════════════
// Scenario: 50 goroutines call BeforeRequest concurrently while the
// breaker is in half-open state with HalfOpenMaxProbes=3.
// Exactly 3 should be admitted; the rest should get ErrUnavailable.
// ═══════════════════════════════════════════════════════════════════
func TestBreaker_ConcurrentHalfOpenProbes(t *testing.T) {
	maxProbes := 3
	cfg := circuitbreaker.Config{
		FailureThreshold:  5,
		SuccessThreshold:  2,
		ResetTimeout:      1 * time.Hour,
		HalfOpenMaxProbes: maxProbes,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test-concurrent", cfg, nil)

	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Now().Add(-2*time.Hour))

	const goroutines = 50
	var admitted atomic.Int32
	var rejected atomic.Int32

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := b.BeforeRequest()
			if err == nil {
				admitted.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}

	close(start)
	wg.Wait()

	admittedCount := int(admitted.Load())
	rejectedCount := int(rejected.Load())

	if admittedCount > maxProbes {
		t.Fatalf("expected at most %d admitted, got %d", maxProbes, admittedCount)
	}
	if admittedCount+rejectedCount != goroutines {
		t.Fatalf("expected %d total, got admitted=%d + rejected=%d",
			goroutines, admittedCount, rejectedCount)
	}
}

// TestBreaker_FullLifecycle validates the complete state machine:
// Closed → Open → HalfOpen → Closed.
func TestBreaker_FullLifecycle(t *testing.T) {
	var transitions []string
	cfg := circuitbreaker.Config{
		FailureThreshold:  2,
		SuccessThreshold:  1,
		ResetTimeout:      10 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("lifecycle", cfg, func(key string, from, to circuitbreaker.State) {
		transitions = append(transitions, from.String()+"→"+to.String())
	})

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker should allow: %v", err)
	}
	b.AfterRequest(errors.New("fail-1"))

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker should allow after 1 failure: %v", err)
	}
	b.AfterRequest(errors.New("fail-2"))

	if err := b.BeforeRequest(); err == nil {
		t.Fatal("open breaker should reject")
	}

	time.Sleep(15 * time.Millisecond)

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("half-open breaker should allow probe: %v", err)
	}

	b.AfterRequest(nil)

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker should allow after recovery: %v", err)
	}

	expectedTransitions := []string{
		"closed→open",
		"open→half-open",
		"half-open→closed",
	}
	if len(transitions) != len(expectedTransitions) {
		t.Fatalf("expected %d transitions, got %d: %v",
			len(expectedTransitions), len(transitions), transitions)
	}
	for i, want := range expectedTransitions {
		if transitions[i] != want {
			t.Fatalf("transition %d: expected %q, got %q", i, want, transitions[i])
		}
	}
}

// TestConfig_WithDefaults_ZeroValues validates that zero-value Config
// gets sensible defaults.
func TestConfig_WithDefaults_ZeroValues(t *testing.T) {
	cfg := circuitbreaker.Config{}.WithDefaults()

	if cfg.FailureThreshold != 5 {
		t.Fatalf("expected FailureThreshold=5, got %d", cfg.FailureThreshold)
	}
	if cfg.SuccessThreshold != 2 {
		t.Fatalf("expected SuccessThreshold=2, got %d", cfg.SuccessThreshold)
	}
	if cfg.ResetTimeout != 30*time.Second {
		t.Fatalf("expected ResetTimeout=30s, got %v", cfg.ResetTimeout)
	}
	if cfg.HalfOpenMaxProbes != 1 {
		t.Fatalf("expected HalfOpenMaxProbes=1, got %d", cfg.HalfOpenMaxProbes)
	}
	if cfg.CountError == nil {
		t.Fatal("expected CountError to be set")
	}
}

// TestBreaker_GetMetrics validates that GetMetrics returns a consistent
// snapshot of breaker state.
func TestBreaker_GetMetrics(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("metrics-test", cfg, nil)

	_ = b.BeforeRequest()
	b.AfterRequest(nil)
	_ = b.BeforeRequest()
	b.AfterRequest(errors.New("fail"))

	m := b.GetMetrics()
	if m.Key != "metrics-test" {
		t.Fatalf("expected key %q, got %q", "metrics-test", m.Key)
	}
	if m.TotalRequests != 2 {
		t.Fatalf("expected 2 requests, got %d", m.TotalRequests)
	}
	if m.TotalSuccesses != 1 {
		t.Fatalf("expected 1 success, got %d", m.TotalSuccesses)
	}
	if m.TotalFailures != 1 {
		t.Fatalf("expected 1 failure, got %d", m.TotalFailures)
	}
	if m.State != "closed" {
		t.Fatalf("expected state 'closed', got %q", m.State)
	}
}
