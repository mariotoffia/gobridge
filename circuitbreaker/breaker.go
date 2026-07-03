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
	// generation counts state transitions. Every admitted request
	// captures the generation it was admitted in (see Token); an outcome
	// reported for an older generation is stale evidence about a
	// previous circuit epoch and is discarded instead of corrupting the
	// current state (e.g. a Closed-era success closing a half-open
	// circuit, or a stale completion stealing a half-open probe slot).
	generation           uint64
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
	// HalfOpenInFlight observability accessor can read it lock-free.
	halfOpenInFlight atomic.Int32
}

// Token is the request handle returned by BeforeRequestToken. It pins
// the breaker generation the request was admitted in so AfterRequestToken
// can discard outcomes that arrive after the circuit has since changed
// state (stale evidence from a previous epoch). The zero Token never
// matches a live generation.
type Token struct {
	generation uint64
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
	}
	for _, opt := range opts {
		opt(b)
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
func (b *Breaker) BeforeRequest() error {
	_, err := b.BeforeRequestToken()
	return err
}

// BeforeRequestToken checks the breaker state and, when the request is
// admitted, returns a Token identifying the circuit generation it was
// admitted in. Pass the Token to AfterRequestToken so an outcome that
// arrives after a state transition is discarded rather than counted
// against a generation it never observed. Returns ErrUnavailable with a
// RetryAfter hint when the circuit is open or the half-open probe quota
// is exhausted.
func (b *Breaker) BeforeRequestToken() (Token, error) {
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
		tok, err := b.tryHalfOpenProbeLocked()
		b.mu.Unlock()
		if notify != nil {
			notify()
		}
		return tok, err
	case StateHalfOpen:
		tok, err := b.tryHalfOpenProbeLocked()
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
// generation are consistent with the admission decision.
func (b *Breaker) tryHalfOpenProbeLocked() (Token, error) {
	if b.halfOpenInFlight.Load() >= int32(b.config.HalfOpenMaxProbes) {
		return Token{}, shared.ErrUnavailable.WithRetryAfter(b.config.ResetTimeout / 2)
	}
	b.halfOpenInFlight.Add(1)
	return Token{generation: b.generation}, nil
}

// AfterRequest records the outcome of a request and transitions state.
//
// Legacy companion to BeforeRequest (ports.CircuitBreaker): the outcome
// is applied to the CURRENT generation because no Token identifies the
// admission generation. Concurrent callers should use
// BeforeRequestToken / AfterRequestToken.
func (b *Breaker) AfterRequest(err error) {
	b.mu.Lock()
	tok := Token{generation: b.generation}
	b.mu.Unlock()
	b.AfterRequestToken(tok, err)
}

// AfterRequestToken records the outcome of a request admitted by
// BeforeRequestToken. An outcome whose Token predates the current
// circuit generation is stale evidence about a previous epoch — it is
// counted in BreakerMetrics.StaleOutcomes and otherwise discarded, so
// it can neither steal a half-open probe slot, nor close the circuit on
// pre-outage successes, nor re-open it on pre-probe failures.
func (b *Breaker) AfterRequestToken(tok Token, err error) {
	countable := err != nil && b.config.CountError(err)

	var notify func()

	b.mu.Lock()
	if tok.generation != b.generation {
		b.staleOutcomes++
		b.mu.Unlock()
		return
	}
	if b.state == StateHalfOpen {
		b.halfOpenInFlight.Add(-1)
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
// before the forced state change are treated as stale.
func (b *Breaker) ForceStateForTest(state State, openedAt time.Time) {
	b.mu.Lock()
	if b.state != state {
		b.generation++
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
