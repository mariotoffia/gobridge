package circuitbreaker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// Generation-token staleness — regression tests
//
// A request admitted in one circuit generation (epoch) must not have
// its outcome applied to a later generation. Before the fix, any
// request completing during HalfOpen decremented the probe slot,
// stale Closed-era successes closed the circuit on pre-outage
// evidence, and stale pre-open failures re-opened a probing circuit.
// ═══════════════════════════════════════════════════════════════════

// tripOpen drives a closed breaker open with countable failures and
// returns the fake clock advanced past the reset timeout so the next
// admission transitions to half-open.
func tripOpenAndCoolDown(t *testing.T, b *circuitbreaker.Breaker, fake *clocktest.Fake, cfg circuitbreaker.Config) {
	t.Helper()
	for i := 0; i < cfg.FailureThreshold; i++ {
		tok, err := b.BeforeRequestToken()
		if err != nil {
			t.Fatalf("failure %d not admitted: %v", i, err)
		}
		b.AfterRequestToken(tok, errors.New("boom"))
	}
	if got := b.GetMetrics().State; got != "open" {
		t.Fatalf("expected open after %d failures, got %s", cfg.FailureThreshold, got)
	}
	fake.Advance(cfg.ResetTimeout + time.Millisecond)
}

// TestBreaker_StaleOutcome_DoesNotStealHalfOpenProbeSlot: a request
// admitted while CLOSED that completes during HALF-OPEN must not free
// (or consume) the probe slot, so a second probe is still rejected
// while the real probe is in flight.
func TestBreaker_StaleOutcome_DoesNotStealHalfOpenProbeSlot(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1, SuccessThreshold: 2,
		ResetTimeout: time.Second, HalfOpenMaxProbes: 1,
	}
	fake := clocktest.New()
	b := circuitbreaker.NewBreaker("stale-slot", cfg, nil, circuitbreaker.WithBreakerClock(fake))

	// Admit a long-running request while closed (epoch 0).
	staleTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("closed breaker rejected request: %v", err)
	}

	// Trip open (epoch 1) and cool down.
	tripOpenAndCoolDown(t, b, fake, cfg)

	// Half-open (epoch 2): the single probe slot is taken.
	probeTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("half-open probe not admitted: %v", err)
	}
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d, want 1", got)
	}

	// The stale closed-era request completes now: it must be discarded,
	// not decrement the probe slot.
	b.AfterRequestToken(staleTok, nil)
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("stale outcome freed the probe slot: HalfOpenInFlight = %d, want 1", got)
	}

	// A second probe must still be rejected while the real probe is out.
	if _, err := b.BeforeRequestToken(); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("second probe admitted past HalfOpenMaxProbes=1, err = %v", err)
	}

	m := b.GetMetrics()
	if m.StaleOutcomes != 1 {
		t.Fatalf("StaleOutcomes = %d, want 1", m.StaleOutcomes)
	}

	// The real probe's outcome still counts.
	b.AfterRequestToken(probeTok, nil)
	if got := b.HalfOpenInFlight(); got != 0 {
		t.Fatalf("real probe did not release the slot: HalfOpenInFlight = %d", got)
	}
}

// TestBreaker_StaleClosedEraSuccess_DoesNotCloseHalfOpen: a success
// recorded for a request admitted BEFORE the outage is pre-outage
// evidence and must not close a half-open circuit.
func TestBreaker_StaleClosedEraSuccess_DoesNotCloseHalfOpen(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1, SuccessThreshold: 1,
		ResetTimeout: time.Second, HalfOpenMaxProbes: 1,
	}
	fake := clocktest.New()
	b := circuitbreaker.NewBreaker("stale-close", cfg, nil, circuitbreaker.WithBreakerClock(fake))

	staleTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("closed breaker rejected request: %v", err)
	}

	tripOpenAndCoolDown(t, b, fake, cfg)

	// Enter half-open by admitting the probe.
	probeTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("half-open probe not admitted: %v", err)
	}

	// Stale success (SuccessThreshold=1 would close immediately if counted).
	b.AfterRequestToken(staleTok, nil)
	if got := b.GetMetrics().State; got != "half-open" {
		t.Fatalf("stale closed-era success changed state to %s, want half-open", got)
	}

	// The genuine probe success closes it.
	b.AfterRequestToken(probeTok, nil)
	if got := b.GetMetrics().State; got != "closed" {
		t.Fatalf("probe success did not close, state = %s", got)
	}
}

// TestBreaker_StalePreOpenFailure_DoesNotReopenHalfOpen: a countable
// failure from a request admitted before the circuit opened must not
// re-open a half-open circuit, and the in-flight probe's success must
// still be honoured (not discarded by a bogus re-open).
func TestBreaker_StalePreOpenFailure_DoesNotReopenHalfOpen(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1, SuccessThreshold: 1,
		ResetTimeout: time.Second, HalfOpenMaxProbes: 1,
	}
	fake := clocktest.New()
	b := circuitbreaker.NewBreaker("stale-reopen", cfg, nil, circuitbreaker.WithBreakerClock(fake))

	staleTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("closed breaker rejected request: %v", err)
	}

	tripOpenAndCoolDown(t, b, fake, cfg)

	probeTok, err := b.BeforeRequestToken()
	if err != nil {
		t.Fatalf("half-open probe not admitted: %v", err)
	}

	// Stale epoch-0 failure arrives while the epoch-2 probe is in flight.
	b.AfterRequestToken(staleTok, errors.New("boom"))
	if got := b.GetMetrics().State; got != "half-open" {
		t.Fatalf("stale failure changed state to %s, want half-open", got)
	}

	// The probe's success must close the circuit — before the fix the
	// bogus re-open advanced the state so this success was discarded.
	b.AfterRequestToken(probeTok, nil)
	if got := b.GetMetrics().State; got != "closed" {
		t.Fatalf("probe success discarded after stale failure, state = %s", got)
	}
}

// TestBreaker_ZeroToken_NeverMatchesLiveGeneration: the Token contract
// says the zero Token never matches a live generation — including on a
// FRESH breaker that has never transitioned. A zero Token (kept from a
// rejected admission, or a zero-value struct field) must be discarded
// as stale, not counted as current-epoch evidence.
func TestBreaker_ZeroToken_NeverMatchesLiveGeneration(t *testing.T) {
	cfg := circuitbreaker.Config{FailureThreshold: 2, SuccessThreshold: 1, ResetTimeout: time.Second}
	b := circuitbreaker.NewBreaker("zero-token", cfg, nil)

	for i := 0; i < 2; i++ {
		b.AfterRequestToken(circuitbreaker.Token{}, errors.New("boom"))
	}

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("zero Tokens tripped a fresh breaker: state = %s, want closed", m.State)
	}
	if m.StaleOutcomes != 2 {
		t.Fatalf("StaleOutcomes = %d, want 2 (zero Token discarded as stale)", m.StaleOutcomes)
	}
	if m.TotalFailures != 0 {
		t.Fatalf("TotalFailures = %d, want 0", m.TotalFailures)
	}
}

// TestBreaker_LegacyAfterRequest_HalfOpen_DoesNotUnderflowProbeSlot: a
// legacy (token-less) AfterRequest landing while HALF-OPEN must not
// release a probe slot it never took. Before the fix the slot count
// went negative, admitting extra concurrent probes past
// HalfOpenMaxProbes onto a still-recovering dependency.
func TestBreaker_LegacyAfterRequest_HalfOpen_DoesNotUnderflowProbeSlot(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1, SuccessThreshold: 5,
		ResetTimeout: time.Second, HalfOpenMaxProbes: 1,
	}
	fake := clocktest.New()
	b := circuitbreaker.NewBreaker("legacy-underflow", cfg, nil, circuitbreaker.WithBreakerClock(fake))

	tripOpenAndCoolDown(t, b, fake, cfg)

	// The single probe slot is taken by a tokened admission.
	if _, err := b.BeforeRequestToken(); err != nil {
		t.Fatalf("half-open probe not admitted: %v", err)
	}
	if got := b.HalfOpenInFlight(); got != 1 {
		t.Fatalf("HalfOpenInFlight = %d, want 1", got)
	}

	// A legacy outcome that never took a probe slot completes now
	// (SuccessThreshold=5 keeps the circuit half-open).
	b.AfterRequest(nil)
	if got := b.HalfOpenInFlight(); got < 0 {
		t.Fatalf("probe slot count underflowed: %d", got)
	}

	// The real probe is still in flight, so a second probe must still
	// be rejected — an underflow would admit it.
	if _, err := b.BeforeRequestToken(); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("second probe admitted past HalfOpenMaxProbes=1, err = %v", err)
	}
}

// TestBreaker_LegacyAfterRequest_StillCounts: the ports.CircuitBreaker
// two-call surface (no token) keeps its sequential-use semantics.
func TestBreaker_LegacyAfterRequest_StillCounts(t *testing.T) {
	cfg := circuitbreaker.Config{FailureThreshold: 2, SuccessThreshold: 1, ResetTimeout: time.Second}
	b := circuitbreaker.NewBreaker("legacy", cfg, nil)

	for i := 0; i < 2; i++ {
		if err := b.BeforeRequest(); err != nil {
			t.Fatalf("request %d rejected: %v", i, err)
		}
		b.AfterRequest(errors.New("boom"))
	}
	if got := b.GetMetrics().State; got != "open" {
		t.Fatalf("legacy failures did not trip breaker, state = %s", got)
	}
}

// ═══════════════════════════════════════════════════════════════════
// Config validation — zero-value and negative configs must not
// produce a threshold-0 / negative-threshold breaker.
// ═══════════════════════════════════════════════════════════════════

func TestConfig_WithDefaults_NegativeValuesNormalised(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold:  -1,
		SuccessThreshold:  -3,
		ResetTimeout:      -time.Second,
		HalfOpenMaxProbes: -2,
	}.WithDefaults()

	if cfg.FailureThreshold != 5 {
		t.Fatalf("FailureThreshold = %d, want 5", cfg.FailureThreshold)
	}
	if cfg.SuccessThreshold != 2 {
		t.Fatalf("SuccessThreshold = %d, want 2", cfg.SuccessThreshold)
	}
	if cfg.ResetTimeout != 30*time.Second {
		t.Fatalf("ResetTimeout = %v, want 30s", cfg.ResetTimeout)
	}
	if cfg.HalfOpenMaxProbes != 1 {
		t.Fatalf("HalfOpenMaxProbes = %d, want 1", cfg.HalfOpenMaxProbes)
	}
	if cfg.CountError == nil {
		t.Fatal("CountError not defaulted")
	}
}

// TestNewBreaker_ZeroValueConfig_GetsDefaults: constructing a breaker
// directly with a zero-value Config must not yield a threshold-0
// breaker that opens on its first observed failure.
func TestNewBreaker_ZeroValueConfig_GetsDefaults(t *testing.T) {
	b := circuitbreaker.NewBreaker("zero-cfg", circuitbreaker.Config{}, nil)

	if got := b.InternalConfig().FailureThreshold; got != 5 {
		t.Fatalf("FailureThreshold = %d, want defaulted 5", got)
	}

	// One failure must NOT open the circuit.
	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker rejected request: %v", err)
	}
	b.AfterRequest(errors.New("boom"))
	if got := b.GetMetrics().State; got != "closed" {
		t.Fatalf("zero-value config opened after one failure, state = %s", got)
	}
}

// TestNewBreaker_NegativeConfig_GetsDefaults: FailureThreshold:-1 must
// not open the circuit on the first failure.
func TestNewBreaker_NegativeConfig_GetsDefaults(t *testing.T) {
	b := circuitbreaker.NewBreaker("neg-cfg", circuitbreaker.Config{FailureThreshold: -1}, nil)

	if err := b.BeforeRequest(); err != nil {
		t.Fatalf("closed breaker rejected request: %v", err)
	}
	b.AfterRequest(errors.New("boom"))
	if got := b.GetMetrics().State; got != "closed" {
		t.Fatalf("negative FailureThreshold opened after one failure, state = %s", got)
	}
}
