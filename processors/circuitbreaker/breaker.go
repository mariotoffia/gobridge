package circuitbreaker

import (
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

type breaker struct {
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

func newBreaker(key string, cfg Config, onStateChange func(string, State, State)) *breaker {
	return &breaker{
		key:           key,
		config:        cfg,
		onStateChange: onStateChange,
	}
}

func (b *breaker) beforeRequest() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(b.openedAt) >= b.config.ResetTimeout {
			b.transitionTo(StateHalfOpen)
			return nil
		}
		remaining := b.config.ResetTimeout - time.Since(b.openedAt)
		return domain.ErrUnavailable.WithRetryAfter(remaining)
	case StateHalfOpen:
		return nil
	default:
		return nil
	}
}

func (b *breaker) afterRequest(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.totalRequests++

	if err != nil {
		b.totalFailures++
		b.consecutiveFailures++
		b.consecutiveSuccesses = 0
		b.lastFailureTime = time.Now()

		if b.state == StateClosed && b.consecutiveFailures >= b.config.FailureThreshold {
			b.transitionTo(StateOpen)
		} else if b.state == StateHalfOpen {
			b.transitionTo(StateOpen)
		}
		return
	}

	b.totalSuccesses++
	b.consecutiveSuccesses++
	b.consecutiveFailures = 0

	if b.state == StateHalfOpen && b.consecutiveSuccesses >= b.config.SuccessThreshold {
		b.transitionTo(StateClosed)
	}
}

func (b *breaker) transitionTo(newState State) {
	if b.state == newState {
		return
	}

	old := b.state
	b.state = newState

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
		b.onStateChange(b.key, old, newState)
	}
}

func (b *breaker) metrics() BreakerMetrics {
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
