package circuitbreaker_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
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

	transientErr := shared.ErrTimeout

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
	b.AfterRequest(shared.ErrTimeout)

	err := b.BeforeRequest()
	if err == nil {
		t.Fatal("open breaker should reject request")
	}

	retryAfter := shared.GetRetryAfter(err)
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
	b.AfterRequest(shared.ErrTimeout)

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

	// Fake clock so no wall-clock time elapses between admissions: the
	// probe-slot reclaim is deadline-driven, and a slow CI could otherwise
	// cross the probe timeout and reclaim the in-flight slot, admitting the
	// "should be rejected" probe.
	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	b := circuitbreaker.NewBreaker("test", cfg, nil, circuitbreaker.WithBreakerClock(fake))
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

// TestBreaker_HalfOpen_AbandonedProbeReclaimed validates that a half-open
// probe slot taken by a caller that never reports an outcome (a missing
// AfterRequestToken defer) is reclaimed after the probe timeout, so the
// breaker cannot wedge half-open and reject every request forever.
func TestBreaker_HalfOpen_AbandonedProbeReclaimed(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  1,
		ResetTimeout:      10 * time.Second,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	b := circuitbreaker.NewBreaker("abandoned", cfg, nil,
		circuitbreaker.WithBreakerClock(fake),
		circuitbreaker.WithHalfOpenProbeTimeout(5*time.Second))
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	// Take the single probe slot and NEVER report its outcome.
	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("first probe should be admitted: %v", err)
	}
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d, want 1", got)
	}

	// The breaker is wedged: a second probe is rejected while the slot is held.
	if _, err := b.BeforeRequestToken(); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("second probe should be rejected while slot held, err = %v", err)
	}

	// Before the timeout elapses the slot is still held.
	fake.Advance(4 * time.Second)
	if _, err := b.BeforeRequestToken(); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("probe should still be rejected before timeout, err = %v", err)
	}

	// Past the probe timeout the abandoned slot is reclaimed and a fresh
	// probe is admitted again — the breaker is no longer wedged.
	fake.Advance(2 * time.Second) // total 6s > 5s probe timeout
	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("probe should be admitted after abandoned slot reclaimed: %v", err)
	}
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d, want 1 (the fresh probe)", got)
	}
}

// TestBreaker_ReclaimedProbeLateOutcomeDoesNotReleaseNewerSlot pins:
// a probe whose slot was reclaimed (it exceeded probe_timeout) must NOT, when it
// finally reports, release the newer probe that has since taken a slot, nor vote
// in the current half-open epoch. Before the fix the token carried no slot
// identity, so the late outcome released the oldest remaining slot — the newer
// probe's — letting the breaker exceed HalfOpenMaxProbes and, worse, letting an
// abandoned probe's late success close the circuit.
//
// Mutation check: revert AfterRequestToken to releaseProbeLocked() (oldest) and
// this fails — the late success releases B's slot (HalfOpenInFlight drops to 0)
// and closes the circuit.
func TestBreaker_ReclaimedProbeLateOutcomeDoesNotReleaseNewerSlot(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  1, // one success would close — so a stray vote is observable
		ResetTimeout:      10 * time.Second,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	b := circuitbreaker.NewBreaker("cb1", cfg, nil,
		circuitbreaker.WithBreakerClock(fake),
		circuitbreaker.WithHalfOpenProbeTimeout(5*time.Second))
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	// Probe A takes the only slot and then goes silent (slow/abandoned).
	tokA, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("probe A should be admitted: %v", err)
	}

	// Past the probe timeout, A's slot is reclaimed and probe B is admitted.
	fake.Advance(6 * time.Second) // > 5s probe timeout
	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("probe B should be admitted after A reclaimed: %v", err)
	}
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d, want 1 (only probe B)", got)
	}

	// A finally reports SUCCESS — a stale outcome for a slot that no longer exists.
	b.AfterRequestToken(tokA, nil)

	m := b.GetMetrics()
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d after A's late outcome, want 1 (B's slot untouched)", got)
	}
	if m.State != circuitbreaker.StateHalfOpen.String() {
		t.Fatalf("state = %q after reclaimed probe's late success, want half-open (must not vote)", m.State)
	}
	if m.StaleOutcomes != 1 {
		t.Fatalf("StaleOutcomes = %d, want 1 (the reclaimed probe's discarded outcome)", m.StaleOutcomes)
	}
	if m.TotalSuccesses != 0 {
		t.Fatalf("TotalSuccesses = %d, want 0 (a reclaimed probe's outcome must not count)", m.TotalSuccesses)
	}
}

// TestBreaker_HalfOpenProbeTimeout_DefaultsFromResetTimeout validates that
// the probe timeout defaults to 2×ResetTimeout when no explicit timeout is
// configured, so an abandoned probe is still eventually reclaimed.
func TestBreaker_HalfOpenProbeTimeout_DefaultsFromResetTimeout(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  1,
		SuccessThreshold:  1,
		ResetTimeout:      3 * time.Second,
		HalfOpenMaxProbes: 1,
	}.WithDefaults()

	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	b := circuitbreaker.NewBreaker("default-timeout", cfg, nil, circuitbreaker.WithBreakerClock(fake))
	b.ForceStateForTest(circuitbreaker.StateHalfOpen, time.Time{})

	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("first probe should be admitted: %v", err)
	}

	// Just under 2×ResetTimeout (6s): still wedged.
	fake.Advance(5 * time.Second)
	if _, err := b.BeforeRequestToken(); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("probe should still be rejected before 2×ResetTimeout, err = %v", err)
	}

	// Past 2×ResetTimeout: reclaimed.
	fake.Advance(2 * time.Second) // total 7s > 6s
	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("probe should be admitted after default probe timeout: %v", err)
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
	b.AfterRequest(shared.ErrTimeout)

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
		b.AfterRequest(shared.ErrInvalidPayload)
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
	b.AfterRequest(shared.ErrTimeout)

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
	b.AfterRequest(shared.ErrTimeout)

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
	} else if retryAfter := shared.GetRetryAfter(err); retryAfter != time.Second {
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
	// the default classifier counts transient/recoverable errors but must
	// NOT count a tenant in-flight quota reject (that is a per-tenant fairness
	// signal, not downstream ill-health — counting it lets one throttled tenant
	// trip a shared breaker and deny every other tenant).
	if cfg.CountError(shared.ErrTenantQuotaExceeded) {
		t.Error("default classifier must NOT count ErrTenantQuotaExceeded")
	}
	if !cfg.CountError(shared.ErrUnavailable) {
		t.Error("default classifier MUST count ErrUnavailable (breaker's core purpose)")
	}
	if !cfg.CountError(shared.ErrTimeout) {
		t.Error("default classifier MUST count ErrTimeout (transient)")
	}
	if cfg.CountError(shared.ErrInvalidPayload) {
		t.Error("default classifier must NOT count ErrInvalidPayload (rejected)")
	}
}

// TestBreaker_TenantQuotaExceeded_DoesNotTrip is the core guard: a burst of
// tenant in-flight quota rejects must leave a shared breaker CLOSED, so one
// throttled tenant cannot deny every other tenant behind the same breaker.
func TestBreaker_TenantQuotaExceeded_DoesNotTrip(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("shared", cfg, nil)

	for i := 0; i < 10; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(shared.ErrTenantQuotaExceeded)
	}

	m := b.GetMetrics()
	if m.State != circuitbreaker.StateClosed.String() {
		t.Fatalf("tenant-quota rejects must not trip the breaker, got state %s", m.State)
	}
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures for quota rejects, got %d", m.ConsecutiveFailures)
	}
}

// TestBreaker_Unavailable_DoesTrip is the regression guard for the breaker's
// core purpose: genuine downstream-unavailable errors MUST still trip it. (A
// nested-breaker-open error is indistinguishable ErrUnavailable and is counted
// by design; see defaultCountError's godoc.)
func TestBreaker_Unavailable_DoesTrip(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("downstream", cfg, nil)

	for i := 0; i < 3; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(shared.ErrUnavailable)
	}

	m := b.GetMetrics()
	if m.State != circuitbreaker.StateOpen.String() {
		t.Fatalf("ErrUnavailable must trip the breaker, got state %s", m.State)
	}
}

// TestBreaker_CustomCountError_Overrides confirms a caller-supplied classifier
// still wins over the tenant-quota-excluding default: here it opts to count
// quota rejects, and the breaker trips on them.
func TestBreaker_CustomCountError_Overrides(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Minute,
		CountError:       func(error) bool { return true },
	}.WithDefaults()

	b := circuitbreaker.NewBreaker("custom", cfg, nil)

	for i := 0; i < 2; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(shared.ErrTenantQuotaExceeded)
	}

	m := b.GetMetrics()
	if m.State != circuitbreaker.StateOpen.String() {
		t.Fatalf("custom CountError must override the default, got state %s", m.State)
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
	b.AfterRequest(shared.ErrTimeout)

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
						b.AfterRequest(shared.ErrTimeout)
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
