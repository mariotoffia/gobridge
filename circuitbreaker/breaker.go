package circuitbreaker

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// Breaker is a standalone circuit breaker that can wrap any
// func() error call pattern. Used internally by the Processor
// and available for transport-level circuit breaking.
type Breaker struct {
	mu                   sync.Mutex
	state                State
	config               Config
	consecutiveFailures  int
	consecutiveSuccesses int
	totalRequests        int64
	totalSuccesses       int64
	totalFailures        int64
	openedAt             time.Time
	lastFailureTime      time.Time
	onStateChange        func(key string, from, to State)
	key                  string
	halfOpenInFlight     atomic.Int32
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
	LastFailureTime      time.Time
}

// NewBreaker creates a Breaker with the given key, config, and optional
// state change callback.
func NewBreaker(key string, cfg Config, onStateChange func(string, State, State)) *Breaker {
	if cfg.HalfOpenMaxProbes <= 0 {
		cfg.HalfOpenMaxProbes = 1
	}
	if cfg.CountError == nil {
		cfg.CountError = domain.IsRecoverableError
	}
	return &Breaker{
		key:           key,
		config:        cfg,
		onStateChange: onStateChange,
	}
}

// BeforeRequest checks the breaker state and returns ErrUnavailable
// with a RetryAfter hint if the circuit is open.
func (b *Breaker) BeforeRequest() error {
	b.mu.Lock()

	switch b.state {
	case StateClosed:
		b.mu.Unlock()
		return nil
	case StateOpen:
		elapsed := time.Since(b.openedAt)
		if elapsed >= b.config.ResetTimeout {
			notify := b.transitionTo(StateHalfOpen)
			b.mu.Unlock()
			if notify != nil {
				notify()
			}
			return b.tryHalfOpenProbe()
		}
		remaining := b.config.ResetTimeout - elapsed
		b.mu.Unlock()
		return domain.ErrUnavailable.WithRetryAfter(remaining)
	case StateHalfOpen:
		b.mu.Unlock()
		return b.tryHalfOpenProbe()
	default:
		b.mu.Unlock()
		return nil
	}
}

func (b *Breaker) tryHalfOpenProbe() error {
	if b.halfOpenInFlight.Add(1) > int32(b.config.HalfOpenMaxProbes) {
		b.halfOpenInFlight.Add(-1)
		return domain.ErrUnavailable.WithRetryAfter(b.config.ResetTimeout / 2)
	}
	return nil
}

// AfterRequest records the outcome of a request and transitions state.
func (b *Breaker) AfterRequest(err error) {
	countable := err != nil && b.config.CountError(err)

	var notify func()

	b.mu.Lock()
	if b.state == StateHalfOpen {
		b.halfOpenInFlight.Add(-1)
	}
	b.totalRequests++

	if err != nil {
		b.totalFailures++
		if countable {
			b.consecutiveFailures++
			b.consecutiveSuccesses = 0
			b.lastFailureTime = time.Now()

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
// the lock. Must be called with b.mu held.
func (b *Breaker) transitionTo(newState State) func() {
	if b.state == newState {
		return nil
	}

	old := b.state
	b.state = newState

	if old == StateHalfOpen {
		b.halfOpenInFlight.Store(0)
	}

	switch newState {
	case StateOpen:
		b.openedAt = time.Now()
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
		LastFailureTime:      b.lastFailureTime,
	}
}

// --- Test helpers (exported for cross-package testing) ---

// ForceStateForTest sets the breaker state directly. Intended for test code only.
func (b *Breaker) ForceStateForTest(state State, openedAt time.Time) {
	b.mu.Lock()
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
