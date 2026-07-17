package circuitbreaker

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Breaker is a standalone circuit breaker that can wrap any
// func() error call pattern. Used internally by the Processor
// and available for transport-level circuit breaking.
type Breaker struct {
	mu    sync.Mutex
	state State
	// generation counts state transitions and starts at 1 so the zero
	// Token (generation 0) can never match a live generation, not even
	// on a fresh breaker. Every admitted request captures the generation
	// it was admitted in (see Token); an outcome reported for another
	// generation is stale evidence about a previous circuit epoch and is
	// discarded instead of corrupting the current state (e.g. a
	// Closed-era success closing a half-open circuit, or a stale
	// completion stealing a half-open probe slot).
	generation uint64
	// legacyOutstanding counts half-open probe slots taken through the
	// token-less BeforeRequest so the token-less AfterRequest can only
	// release a slot the legacy surface actually took. Without it, a
	// legacy outcome landing during half-open released a slot it never
	// acquired, underflowing the probe accounting and admitting extra
	// concurrent probes past HalfOpenMaxProbes. Guarded by mu.
	legacyOutstanding    int
	config               Config
	consecutiveFailures  int
	consecutiveSuccesses int
	totalRequests        int64
	totalSuccesses       int64
	totalFailures        int64
	staleOutcomes        int64
	openedAt             time.Time
	lastFailureTime      time.Time
	onStateChange        func(key string, from, to State)
	key                  string
	clk                  clock.Clock
	// halfOpenInFlight is mutated only under mu; it stays atomic so the
	// HalfOpenInFlight observability accessor can read it lock-free. It is
	// kept equal to len(halfOpenProbes) after every mutation.
	halfOpenInFlight atomic.Int32
	// halfOpenProbes tracks one entry per outstanding half-open probe slot,
	// each stamped with an admission deadline, so an abandoned probe (a
	// caller that took a slot via BeforeRequest[Token] but never reported an
	// outcome) cannot wedge the breaker half-open forever. Expired slots are
	// reclaimed on the next admission attempt (reclaimExpiredProbesLocked).
	// Ordered by admission, hence by deadline (the clock is monotonic), so
	// the expired entries form a prefix. Guarded by mu.
	halfOpenProbes []probeSlot
	// probeTimeout is how long a half-open probe slot may stay outstanding
	// before it is reclaimed. Defaults to 2×ResetTimeout (see NewBreaker) and
	// is overridable via WithHalfOpenProbeTimeout.
	probeTimeout time.Duration
}

// probeSlot records one outstanding half-open probe admission: the
// deadline after which the slot is reclaimed, and whether it was taken
// through the token-less legacy surface (so reclaim keeps legacyOutstanding
// consistent).
type probeSlot struct {
	deadline time.Time
	legacy   bool
}

// Token is the request handle returned by BeforeRequestToken. It pins
// the breaker generation the request was admitted in so AfterRequestToken
// can discard outcomes that arrive after the circuit has since changed
// state (stale evidence from a previous epoch), and records whether the
// admission took a half-open probe slot so only genuine probe outcomes
// release one. The zero Token never matches a live generation (breaker
// generations start at 1).
type Token struct {
	generation uint64
	// probe is true when the admission consumed a half-open probe slot;
	// AfterRequestToken releases a slot only for such tokens.
	probe bool
}

// BreakerOption configures a Breaker.
type BreakerOption func(*Breaker)

// WithBreakerClock sets the clock used for breaker timestamps and elapsed-time checks.
func WithBreakerClock(c clock.Clock) BreakerOption {
	return func(b *Breaker) {
		if c != nil {
			b.clk = c
		}
	}
}

// WithHalfOpenProbeTimeout sets how long a half-open probe slot may stay
// outstanding before it is reclaimed (so an abandoned probe cannot wedge
// the breaker half-open forever). A non-positive value is ignored, leaving
// the default of 2×ResetTimeout.
//
// PRECONDITION: callers MUST bound their own request timeout below this
// value. Reclaim treats a slot past its deadline as an abandoned caller and
// frees it; if a probe is genuinely still in flight when that happens (a
// downstream slower than probeTimeout and a caller with no shorter timeout),
// the freed slot admits another probe and HalfOpenMaxProbes stops capping
// concurrency — the breaker can then pile probes onto a slow-but-alive
// dependency, the exact thundering herd it exists to prevent. Keep
// probeTimeout larger than the worst-case admitted call so a live probe
// always reports before its deadline.
func WithHalfOpenProbeTimeout(d time.Duration) BreakerOption {
	return func(b *Breaker) {
		if d > 0 {
			b.probeTimeout = d
		}
	}
}

// BreakerMetrics is a point-in-time snapshot of a single circuit breaker's counters and state.
type BreakerMetrics struct {
	Key                  string
	State                string
	TotalRequests        int64
	TotalSuccesses       int64
	TotalFailures        int64
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	// StaleOutcomes counts AfterRequestToken calls whose Token belonged
	// to a previous circuit generation and were therefore discarded.
	StaleOutcomes   int64
	LastFailureTime time.Time
}

// NewBreaker creates a Breaker with the given key, config, optional
// state change callback, and options. The config is normalised via
// Config.WithDefaults, so a zero-value (or negative-valued) Config gets
// the documented defaults instead of a threshold-0 breaker that opens
// on its first observation.
func NewBreaker(key string, cfg Config, onStateChange func(string, State, State), opts ...BreakerOption) *Breaker {
	b := &Breaker{
		key:           key,
		config:        cfg.WithDefaults(),
		onStateChange: onStateChange,
		clk:           clock.System,
		// Start at 1 so the zero Token can never match a live
		// generation (see Token).
		generation: 1,
	}
	for _, opt := range opts {
		opt(b)
	}
	// Default the probe-slot reclaim deadline to 2×ResetTimeout: long enough
	// that a legitimately slow probe is not reclaimed out from under a
	// recovering dependency, short enough that an abandoned probe (a missing
	// AfterRequest[Token]) cannot wedge the breaker half-open indefinitely.
	if b.probeTimeout <= 0 {
		b.probeTimeout = 2 * b.config.ResetTimeout
	}
	return b
}

// BeforeRequest checks the breaker state and returns ErrUnavailable
// with a RetryAfter hint if the circuit is open.
//
// It satisfies ports.CircuitBreaker for sequential per-request use.
// Callers that may have multiple requests in flight on one breaker
// should use BeforeRequestToken / AfterRequestToken instead: without a
// Token, AfterRequest cannot detect that the circuit changed state
// while the request was in flight, so a stale outcome is applied to the
// current generation.
//
// Every admitted request MUST be paired with exactly one AfterRequest
// (ideally via defer): an admitted half-open probe that never reports an
// outcome holds its slot until the probe timeout elapses.
func (b *Breaker) BeforeRequest() error {
	_, err := b.beforeRequest(true)
	return err
}

// BeforeRequestToken checks the breaker state and, when the request is
// admitted, returns a Token identifying the circuit generation it was
// admitted in. Pass the Token to AfterRequestToken so an outcome that
// arrives after a state transition is discarded rather than counted
// against a generation it never observed. Returns ErrUnavailable with a
// RetryAfter hint when the circuit is open or the half-open probe quota
// is exhausted.
//
// Every admitted request MUST be paired with exactly one AfterRequestToken
// (ideally via defer): an admitted half-open probe that never reports an
// outcome holds its slot until the probe timeout elapses (see
// WithHalfOpenProbeTimeout).
func (b *Breaker) BeforeRequestToken() (Token, error) {
	return b.beforeRequest(false)
}

// beforeRequest implements admission for both API surfaces. legacy is
// true for the token-less BeforeRequest, whose half-open probe slots
// are counted in legacyOutstanding — inside the same critical section
// as the admission — so the token-less AfterRequest can only release a
// slot this surface actually took.
func (b *Breaker) beforeRequest(legacy bool) (Token, error) {
	b.mu.Lock()

	switch b.state {
	case StateOpen:
		elapsed := b.clk.Since(b.openedAt)
		if elapsed < b.config.ResetTimeout {
			remaining := b.config.ResetTimeout - elapsed
			b.mu.Unlock()
			return Token{}, shared.ErrUnavailable.WithRetryAfter(remaining)
		}
		notify := b.transitionTo(StateHalfOpen)
		tok, err := b.tryHalfOpenProbeLocked(legacy)
		b.mu.Unlock()
		if notify != nil {
			notify()
		}
		return tok, err
	case StateHalfOpen:
		tok, err := b.tryHalfOpenProbeLocked(legacy)
		b.mu.Unlock()
		return tok, err
	default: // StateClosed and any unknown state admit the request.
		tok := Token{generation: b.generation}
		b.mu.Unlock()
		return tok, nil
	}
}

// tryHalfOpenProbeLocked admits a half-open probe when a slot is free.
// Must be called with b.mu held so the slot accounting and the returned
// generation are consistent with the admission decision. legacy probes
// are additionally counted in legacyOutstanding (see beforeRequest).
//
// It first reclaims any probe slots whose admission deadline has elapsed
// so an abandoned probe (a caller that never reported an outcome) cannot
// hold a slot — and thus wedge the breaker half-open — forever.
func (b *Breaker) tryHalfOpenProbeLocked(legacy bool) (Token, error) {
	b.reclaimExpiredProbesLocked()
	if len(b.halfOpenProbes) >= b.config.HalfOpenMaxProbes {
		return Token{}, shared.ErrUnavailable.WithRetryAfter(b.config.ResetTimeout / 2)
	}
	b.halfOpenProbes = append(b.halfOpenProbes, probeSlot{
		deadline: b.clk.Now().Add(b.probeTimeout),
		legacy:   legacy,
	})
	b.halfOpenInFlight.Store(int32(len(b.halfOpenProbes)))
	if legacy {
		b.legacyOutstanding++
	}
	return Token{generation: b.generation, probe: true}, nil
}

// reclaimExpiredProbesLocked drops probe slots whose admission deadline
// has elapsed. A caller that takes a half-open probe but never reports an
// outcome (a missing AfterRequestToken/AfterRequest — see their godoc)
// would otherwise hold its slot forever and wedge the breaker half-open,
// rejecting every request. Slots are admission-ordered, hence
// deadline-ordered (the clock is monotonic), so the expired ones form a
// prefix. Reclaiming does NOT advance the generation: a healthy concurrent
// probe (with HalfOpenMaxProbes>1) keeps its slot and its outcome still
// counts. By the same token, an outcome that arrives AFTER its slot was
// reclaimed still matches the generation and is counted — a belated success
// is still a success, a belated failure still real evidence; reclaim frees
// the slot for concurrency, it does not disqualify the vote. Must be called
// with b.mu held.
func (b *Breaker) reclaimExpiredProbesLocked() {
	if len(b.halfOpenProbes) == 0 {
		return
	}
	now := b.clk.Now()
	n := 0
	for n < len(b.halfOpenProbes) && !b.halfOpenProbes[n].deadline.After(now) {
		if b.halfOpenProbes[n].legacy && b.legacyOutstanding > 0 {
			b.legacyOutstanding--
		}
		n++
	}
	if n == 0 {
		return
	}
	b.halfOpenProbes = b.halfOpenProbes[n:]
	b.halfOpenInFlight.Store(int32(len(b.halfOpenProbes)))
}

// releaseProbeLocked releases the oldest outstanding half-open probe slot,
// if any. It is a no-op when no slots remain — e.g. the slot was already
// reclaimed by reclaimExpiredProbesLocked after an abandoned probe's
// deadline elapsed — so a late outcome can never underflow the probe
// accounting. Must be called with b.mu held.
func (b *Breaker) releaseProbeLocked() {
	if len(b.halfOpenProbes) == 0 {
		return
	}
	slot := b.halfOpenProbes[0]
	b.halfOpenProbes = b.halfOpenProbes[1:]
	if slot.legacy && b.legacyOutstanding > 0 {
		b.legacyOutstanding--
	}
	b.halfOpenInFlight.Store(int32(len(b.halfOpenProbes)))
}

// AfterRequest records the outcome of a request and transitions state.
//
// Legacy companion to BeforeRequest (ports.CircuitBreaker): the outcome
// is applied to the CURRENT generation because no Token identifies the
// admission generation. A half-open probe slot is released only when
// this legacy surface has one outstanding (taken by BeforeRequest), so
// a token-less outcome can never underflow the probe accounting and
// admit extra concurrent probes. Concurrent callers should use
// BeforeRequestToken / AfterRequestToken.
//
// Callers MUST invoke AfterRequest exactly once for every admitted
// BeforeRequest (ideally via defer). A missing call leaves a half-open
// probe slot outstanding; it is only reclaimed after the probe timeout
// elapses (see WithHalfOpenProbeTimeout), during which the breaker admits
// no further probes.
func (b *Breaker) AfterRequest(err error) {
	b.mu.Lock()
	tok := Token{generation: b.generation}
	if b.state == StateHalfOpen {
		// Reclaim first so a stale slot does not make this outcome look
		// like it holds a probe, then release only if the legacy surface
		// actually has one outstanding.
		b.reclaimExpiredProbesLocked()
		if b.legacyOutstanding > 0 {
			tok.probe = true
		}
	}
	b.mu.Unlock()
	b.AfterRequestToken(tok, err)
}

// AdmitRequest implements ports.CircuitBreakerAdmitter: the generation-safe
// admission surface for callers with multiple requests in flight on one
// breaker. It is BeforeRequestToken/AfterRequestToken with the Token carried
// inside the settle closure, so port-side adapters get stale-outcome
// discarding without the concrete Token type crossing the port boundary.
func (b *Breaker) AdmitRequest() (func(error), error) {
	tok, err := b.BeforeRequestToken()
	if err != nil {
		return nil, err
	}
	return func(outcome error) { b.AfterRequestToken(tok, outcome) }, nil
}

// AfterRequestToken records the outcome of a request admitted by
// BeforeRequestToken. An outcome whose Token predates the current
// circuit generation is stale evidence about a previous epoch — it is
// counted in BreakerMetrics.StaleOutcomes and otherwise discarded, so
// it can neither steal a half-open probe slot, nor close the circuit on
// pre-outage successes, nor re-open it on pre-probe failures.
//
// Callers MUST invoke AfterRequestToken exactly once for every Token
// returned by BeforeRequestToken (ideally via defer). A missing call
// leaves a half-open probe slot outstanding; it is only reclaimed after
// the probe timeout elapses (see WithHalfOpenProbeTimeout), during which
// the breaker admits no further probes.
func (b *Breaker) AfterRequestToken(tok Token, err error) {
	countable := err != nil && b.config.CountError(err)

	var notify func()

	b.mu.Lock()
	if tok.generation != b.generation {
		b.staleOutcomes++
		b.mu.Unlock()
		return
	}
	if tok.probe {
		// The generation matched, so no transition has happened since
		// the admission: the breaker is still half-open and this token
		// holds one of its probe slots. Only probe tokens release a
		// slot, and releaseProbeLocked no-ops when the slot was already
		// reclaimed, so an outcome that never took one (a closed-era
		// admission or a token-less legacy call) — or a late outcome for
		// an already-reclaimed probe — cannot underflow the accounting.
		b.releaseProbeLocked()
	}
	b.totalRequests++

	if err != nil {
		b.totalFailures++
		if countable {
			b.consecutiveFailures++
			b.consecutiveSuccesses = 0
			b.lastFailureTime = b.clk.Now()

			if b.state == StateClosed && b.consecutiveFailures >= b.config.FailureThreshold {
				notify = b.transitionTo(StateOpen)
			} else if b.state == StateHalfOpen {
				notify = b.transitionTo(StateOpen)
			}
		}
		b.mu.Unlock()
		if notify != nil {
			notify()
		}
		return
	}

	b.totalSuccesses++
	b.consecutiveSuccesses++
	b.consecutiveFailures = 0

	if b.state == StateHalfOpen && b.consecutiveSuccesses >= b.config.SuccessThreshold {
		notify = b.transitionTo(StateClosed)
	}
	b.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// transitionTo changes state and returns a callback to invoke AFTER releasing
// the lock. Must be called with b.mu held. Every transition advances the
// breaker generation so outcomes admitted before the transition are
// recognisable as stale (see AfterRequestToken).
func (b *Breaker) transitionTo(newState State) func() {
	if b.state == newState {
		return nil
	}

	old := b.state
	b.state = newState
	b.generation++

	if old == StateHalfOpen {
		b.halfOpenInFlight.Store(0)
		b.legacyOutstanding = 0
		b.halfOpenProbes = nil
	}

	switch newState {
	case StateOpen:
		b.openedAt = b.clk.Now()
	case StateClosed:
		b.consecutiveFailures = 0
		b.consecutiveSuccesses = 0
	case StateHalfOpen:
		b.consecutiveSuccesses = 0
	}

	if b.onStateChange != nil {
		key := b.key
		cb := b.onStateChange
		return func() { cb(key, old, newState) }
	}
	return nil
}

// GetMetrics returns a point-in-time snapshot of this breaker's counters.
func (b *Breaker) GetMetrics() BreakerMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()

	return BreakerMetrics{
		Key:                  b.key,
		State:                b.state.String(),
		TotalRequests:        b.totalRequests,
		TotalSuccesses:       b.totalSuccesses,
		TotalFailures:        b.totalFailures,
		ConsecutiveFailures:  b.consecutiveFailures,
		ConsecutiveSuccesses: b.consecutiveSuccesses,
		StaleOutcomes:        b.staleOutcomes,
		LastFailureTime:      b.lastFailureTime,
	}
}

// --- Test helpers (exported for cross-package testing) ---

// ForceStateForTest sets the breaker state directly. Intended for test code
// only. Advances the generation like a real transition so outcomes admitted
// before the forced state change are treated as stale, and resets the
// half-open probe accounting when leaving half-open.
func (b *Breaker) ForceStateForTest(state State, openedAt time.Time) {
	b.mu.Lock()
	if b.state != state {
		b.generation++
		if b.state == StateHalfOpen {
			b.halfOpenInFlight.Store(0)
			b.legacyOutstanding = 0
			b.halfOpenProbes = nil
		}
	}
	b.state = state
	b.openedAt = openedAt
	b.mu.Unlock()
}

// SetOnStateChangeForTest replaces the state change callback after construction.
func (b *Breaker) SetOnStateChangeForTest(fn func(string, State, State)) {
	b.mu.Lock()
	b.onStateChange = fn
	b.mu.Unlock()
}

// HalfOpenInFlight returns the current number of in-flight half-open probes.
func (b *Breaker) HalfOpenInFlight() int32 {
	return b.halfOpenInFlight.Load()
}

// InternalConfig returns the breaker's internal configuration for assertions.
func (b *Breaker) InternalConfig() Config {
	return b.config
}
