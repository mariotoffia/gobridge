package circuitbreaker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// ═══════════════════════════════════════════════════════════════════
// Circuit Breaker State Machine — Co-located Unit Tests
//
// Covers the standalone Breaker API: state transitions, half-open
// probing, metrics, configuration defaults, and edge cases
// identified by expert review.
//
// Summary:
// ┌──────┬────────────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                        │ Type     │
// ├──────┼────────────────────────────────────────────────────┼──────────┤
// │ T001 │ Closed state allows requests                       │ unit     │
// │ T002 │ Transitions closed->open after threshold           │ unit     │
// │ T003 │ Open state rejects with RetryAfter                 │ unit     │
// │ T004 │ Open transitions to half-open after ResetTimeout   │ unit     │
// │ T005 │ Half-open limits concurrent probes                 │ unit     │
// │ T006 │ Half-open success transitions to closed            │ unit     │
// │ T007 │ Half-open failure reopens circuit                  │ unit     │
// │ T008 │ Non-countable errors don't trip breaker            │ unit     │
// │ T009 │ GetMetrics returns consistent snapshot             │ unit     │
// │ T010 │ WithDefaults fills zero-value config               │ unit     │
// │ T011 │ State.String() covers all states                   │ unit     │
// │ T012 │ State change callback invoked outside lock         │ unit     │
// └──────┴────────────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════

// TestBreaker_Closed_AllowsRequests validates that a newly created breaker
// in closed state permits all requests.
func TestBreaker_Closed_AllowsRequests(t *testing.T) {
	b := circuitbreaker.NewBreaker("test", circuitbreaker.DefaultConfig(), nil)

	for i := 0; i < 10; i++ {
		if err := b.BeforeRequest(); err != nil {
			t.Fatalf("iteration %d: closed breaker rejected request: %v", i, err)
		}
		b.AfterRequest(nil)
	}

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("expected state closed, got %s", m.State)
	}
	if m.TotalRequests != 10 {
		t.Fatalf("expected 10 total requests, got %d", m.TotalRequests)
	}
	if m.TotalSuccesses != 10 {
		t.Fatalf("expected 10 successes, got %d", m.TotalSuccesses)
	}
}

// TestBreaker_ClosedToOpen_AfterThreshold validates that the breaker
// transitions from closed to open after FailureThreshold consecutive
// countable errors.
func TestBreaker_ClosedToOpen_AfterThreshold(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
	}.WithDefaults()

	var transitions []string
	b := circuitbreaker.NewBreaker("test", cfg, func(key string, from, to circuitbreaker.State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	})

	transientErr := domain.ErrTimeout

	for i := 0; i < 2; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(transientErr)
	}
	if m := b.GetMetrics(); m.State != "closed" {
		t.Fatalf("after 2 failures: expected closed, got %s", m.State)
	}

	_ = b.BeforeRequest()
	b.AfterRequest(transientErr)

	if m := b.GetMetrics(); m.State != "open" {
		t.Fatalf("after 3 failures: expected open, got %s", m.State)
	}
	if len(transitions) != 1 || transitions[0] != "closed->open" {
		t.Fatalf("expected [closed->open], got %v", transitions)
	}
}

// TestBreaker_Open_RejectsWithRetryAfter validates that an open breaker
// returns ErrUnavailable with a RetryAfter hint.
func TestBreaker_Open_RejectsWithRetryAfter(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Second,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test", cfg, nil)

	_ = b.BeforeRequest()
	b.AfterRequest(domain.ErrTimeout)

	err := b.BeforeRequest()
	if err == nil {
		t.Fatal("open breaker should reject request")
	}

	retryAfter := domain.GetRetryAfter(err)
	if retryAfter <= 0 || retryAfter > 10*time.Second {
		t.Fatalf("expected RetryAfter in (0, 10s], got %v", retryAfter)
	}
}

// TestBreaker_OpenToHalfOpen_AfterResetTimeout validates that an open
// breaker transitions to half-open after ResetTimeout elapses.
func TestBreaker_OpenToHalfOpen_AfterResetTimeout(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  1,
		ResetTimeout:      50 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()
	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))

	var transitions []string
	b := circuitbreaker.NewBreaker("test", cfg, func(key string, from, to circuitbreaker.State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	}, circuitbreaker.WithBreakerClock(fake))

	_ = b.BeforeRequest()
	b.AfterRequest(domain.ErrTimeout)

	fake.Advance(100 * time.Millisecond)

	err := b.BeforeRequest()
	if err != nil {
		t.Fatalf("should admit probe after ResetTimeout, got: %v", err)
	}
}

// TestBreaker_HalfOpen_LimitsProbes validates that the half-open state
// limits concurrent probes to HalfOpenMaxProbes.
func TestBreaker_HalfOpen_LimitsProbes(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  2,
		ResetTimeout:      50 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test", cfg, nil)
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	err1 := b.BeforeRequest()
	if err1 != nil {
		t.Fatalf("first probe should be admitted: %v", err1)
	}

	err2 := b.BeforeRequest()
	if err2 == nil {
		t.Fatal("second probe should be rejected when max probes = 1")
	}

	b.AfterRequest(nil)

	err3 := b.BeforeRequest()
	if err3 != nil {
		t.Fatalf("probe after completion should be admitted: %v", err3)
	}
}

// TestBreaker_HalfOpenSuccess_TransitionsToClosed validates that
// SuccessThreshold consecutive successes in half-open state close the circuit.
func TestBreaker_HalfOpenSuccess_TransitionsToClosed(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  3,
		SuccessThreshold:  2,
		ResetTimeout:      50 * time.Millisecond,
		HalfOpenMaxProbes: 2,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test", cfg, nil)
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	_ = b.BeforeRequest()
	b.AfterRequest(nil)

	_ = b.BeforeRequest()
	b.AfterRequest(nil)

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("expected closed after %d successes, got %s", cfg.SuccessThreshold, m.State)
	}
}

// TestBreaker_HalfOpenFailure_ReopensCircuit validates that a countable
// failure in half-open state transitions back to open.
func TestBreaker_HalfOpenFailure_ReopensCircuit(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  3,
		SuccessThreshold:  2,
		ResetTimeout:      50 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test", cfg, nil)
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	_ = b.BeforeRequest()
	b.AfterRequest(domain.ErrTimeout)

	m := b.GetMetrics()
	if m.State != "open" {
		t.Fatalf("expected open after half-open failure, got %s", m.State)
	}
}

// TestBreaker_NonCountableError_DoesNotTrip validates that errors where
// CountError returns false do not increment the consecutive failure counter.
func TestBreaker_NonCountableError_DoesNotTrip(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
		CountError:       func(err error) bool { return false },
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("test", cfg, nil)

	for i := 0; i < 10; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(domain.ErrInvalidPayload)
	}

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("non-countable errors should not trip breaker, got state %s", m.State)
	}
	if m.TotalFailures != 10 {
		t.Fatalf("expected 10 total failures counted, got %d", m.TotalFailures)
	}
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures, got %d", m.ConsecutiveFailures)
	}
}

// TestBreaker_GetMetrics_Snapshot validates that GetMetrics returns
// a consistent point-in-time snapshot with all fields populated.
func TestBreaker_GetMetrics_Snapshot(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("my-service", cfg, nil)

	_ = b.BeforeRequest()
	b.AfterRequest(nil)
	_ = b.BeforeRequest()
	b.AfterRequest(domain.ErrTimeout)

	m := b.GetMetrics()
	if m.Key != "my-service" {
		t.Fatalf("expected key 'my-service', got %q", m.Key)
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
	if m.ConsecutiveFailures != 1 {
		t.Fatalf("expected 1 consecutive failure, got %d", m.ConsecutiveFailures)
	}
	if m.LastFailureTime.IsZero() {
		t.Fatal("expected non-zero LastFailureTime after a failure")
	}
}

func TestBreaker_WithBreakerClockControlsFailureAndResetTiming(t *testing.T) {
	start := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	fake := clocktest.NewAt(start)
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  1,
		ResetTimeout:      10 * time.Second,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("clocked", cfg, nil, circuitbreaker.WithBreakerClock(fake))

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker should allow request: %v", err)
	}
	b.AfterRequest(domain.ErrTimeout)

	m := b.GetMetrics()
	if !m.LastFailureTime.Equal(start) {
		t.Fatalf("LastFailureTime = %v, want %v", m.LastFailureTime, start)
	}
	if m.State != circuitbreaker.StateOpen.String() {
		t.Fatalf("state = %s, want open", m.State)
	}

	fake.Advance(9 * time.Second)
	if err := b.BeforeRequest(); err == nil {
		t.Fatal("open breaker should reject before injected ResetTimeout elapses")
	} else if retryAfter := domain.GetRetryAfter(err); retryAfter != time.Second {
		t.Fatalf("RetryAfter = %v, want 1s", retryAfter)
	}

	fake.Advance(time.Second)
	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("breaker should allow half-open probe after injected ResetTimeout: %v", err)
	}
}

// TestConfig_WithDefaults validates that zero-valued fields are filled
// with sensible defaults.
func TestConfig_WithDefaults(t *testing.T) {
	cfg := circuitbreaker.Config{}.WithDefaults()

	if cfg.FailureThreshold != 5 {
		t.Fatalf("expected default FailureThreshold 5, got %d", cfg.FailureThreshold)
	}
	if cfg.SuccessThreshold != 2 {
		t.Fatalf("expected default SuccessThreshold 2, got %d", cfg.SuccessThreshold)
	}
	if cfg.ResetTimeout != 30*time.Second {
		t.Fatalf("expected default ResetTimeout 30s, got %v", cfg.ResetTimeout)
	}
	if cfg.HalfOpenMaxProbes != 1 {
		t.Fatalf("expected default HalfOpenMaxProbes 1, got %d", cfg.HalfOpenMaxProbes)
	}
	if cfg.CountError == nil {
		t.Fatal("expected default CountError to be non-nil")
	}
}

// TestState_String validates the string representation of all states
// including the unknown default branch.
func TestState_String(t *testing.T) {
	tests := []struct {
		state circuitbreaker.State
		want  string
	}{
		{circuitbreaker.StateClosed, "closed"},
		{circuitbreaker.StateOpen, "open"},
		{circuitbreaker.StateHalfOpen, "half-open"},
		{circuitbreaker.State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// TestBreaker_StateChangeCallback_InvokedOutsideLock validates that
// the state change callback is invoked after the mutex is released,
// preventing deadlocks if the callback re-enters the breaker.
func TestBreaker_StateChangeCallback_InvokedOutsideLock(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
	}.WithDefaults()

	callbackCalled := make(chan struct{}, 1)
	var b *circuitbreaker.Breaker
	b = circuitbreaker.NewBreaker("test", cfg, func(key string, from, to circuitbreaker.State) {
		_ = b.GetMetrics()
		callbackCalled <- struct{}{}
	})

	_ = b.BeforeRequest()
	b.AfterRequest(domain.ErrTimeout)

	select {
	case <-callbackCalled:
	case <-time.After(time.Second):
		t.Fatal("state change callback was not invoked within 1s")
	}
}

// TestBreaker_ConcurrentAccess validates that the breaker is safe for
// concurrent use by multiple goroutines.
func TestBreaker_ConcurrentAccess(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  10,
		SuccessThreshold:  5,
		ResetTimeout:      50 * time.Millisecond,
		HalfOpenMaxProbes: 3,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("concurrent", cfg, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				err := b.BeforeRequest()
				if err == nil {
					if n%2 == 0 {
						b.AfterRequest(nil)
					} else {
						b.AfterRequest(domain.ErrTimeout)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	m := b.GetMetrics()
	if m.TotalRequests == 0 {
		t.Fatal("expected some requests to be processed")
	}
}
