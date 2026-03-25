// Package types provides the circuit breaker types and interfaces.
package types

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// Circuit Breaker Errors
// ============================================================================

var (
	// ErrCircuitOpen is returned when the circuit breaker is open.
	ErrCircuitOpen = errors.New("circuit breaker is open")
	// ErrCircuitHalfOpen is returned when too many requests during half-open state.
	ErrCircuitHalfOpen = errors.New("circuit breaker is half-open, limited requests allowed")
)

// ============================================================================
// Circuit Breaker State
// ============================================================================

// CircuitState represents the current state of a circuit breaker.
type CircuitState int32

const (
	// CircuitClosed is the normal state where requests are allowed.
	CircuitClosed CircuitState = iota
	// CircuitOpen is the state where all requests are rejected.
	CircuitOpen
	// CircuitHalfOpen is the state where limited requests are allowed to test recovery.
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ============================================================================
// Circuit Breaker Configuration
// ============================================================================

// CircuitBreakerConfig configures a circuit breaker.
type CircuitBreakerConfig struct {
	// Name identifies this circuit breaker (for metrics/logging).
	Name string `json:"name"`

	// FailureThreshold is the number of consecutive failures before opening the circuit.
	// Default: 5
	FailureThreshold int `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the number of consecutive successes in half-open state
	// before closing the circuit.
	// Default: 2
	SuccessThreshold int `json:"successThreshold,omitempty"`

	// OpenTimeout is how long the circuit stays open before transitioning to half-open.
	// Default: 30s
	OpenTimeout time.Duration `json:"openTimeout,omitempty"`

	// HalfOpenMaxRequests is the maximum number of requests allowed in half-open state.
	// Default: 1
	HalfOpenMaxRequests int `json:"halfOpenMaxRequests,omitempty"`

	// SlidingWindowSize is the size of the sliding window for failure counting.
	// If 0, uses consecutive failure counting instead.
	// Default: 0 (consecutive counting)
	SlidingWindowSize int `json:"slidingWindowSize,omitempty"`

	// SlidingWindowDuration is the time window for sliding window failure counting.
	// Default: 60s
	SlidingWindowDuration time.Duration `json:"slidingWindowDuration,omitempty"`

	// FailureRateThreshold is the failure percentage (0-100) that triggers opening.
	// Only used with sliding window. Default: 50
	FailureRateThreshold float64 `json:"failureRateThreshold,omitempty"`

	// OnStateChange is called when the circuit state changes.
	OnStateChange func(name string, from, to CircuitState) `json:"-"`

	// IsFailure determines if an error should count as a failure.
	// If nil, all non-nil errors are failures.
	IsFailure func(err error) bool `json:"-"`
}

// DefaultCircuitBreakerConfig returns a default configuration.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:      5,
		SuccessThreshold:      2,
		OpenTimeout:           30 * time.Second,
		HalfOpenMaxRequests:   1,
		SlidingWindowDuration: 60 * time.Second,
		FailureRateThreshold:  50,
	}
}

// WithDefaults applies default values for zero fields.
func (c CircuitBreakerConfig) WithDefaults() CircuitBreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 30 * time.Second
	}
	if c.HalfOpenMaxRequests <= 0 {
		c.HalfOpenMaxRequests = 1
	}
	if c.SlidingWindowDuration <= 0 {
		c.SlidingWindowDuration = 60 * time.Second
	}
	if c.FailureRateThreshold <= 0 {
		c.FailureRateThreshold = 50
	}
	return c
}

// ============================================================================
// Circuit Breaker Interface
// ============================================================================

// CircuitBreaker provides circuit breaker functionality for protecting external calls.
type CircuitBreaker interface {
	// Execute runs the given function with circuit breaker protection.
	// Returns ErrCircuitOpen if the circuit is open.
	Execute(ctx context.Context, fn func(ctx context.Context) error) error

	// State returns the current circuit state.
	State() CircuitState

	// Metrics returns circuit breaker metrics.
	Metrics() CircuitBreakerMetrics

	// Reset resets the circuit breaker to closed state.
	Reset()

	// Name returns the circuit breaker name.
	Name() string
}

// CircuitBreakerMetrics contains circuit breaker statistics.
type CircuitBreakerMetrics struct {
	// Name is the circuit breaker name.
	Name string `json:"name"`
	// State is the current state.
	State string `json:"state"`
	// TotalRequests is the total number of requests.
	TotalRequests int64 `json:"totalRequests"`
	// TotalSuccesses is the total successful requests.
	TotalSuccesses int64 `json:"totalSuccesses"`
	// TotalFailures is the total failed requests.
	TotalFailures int64 `json:"totalFailures"`
	// ConsecutiveSuccesses is the current consecutive success count.
	ConsecutiveSuccesses int `json:"consecutiveSuccesses"`
	// ConsecutiveFailures is the current consecutive failure count.
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// LastFailureTime is when the last failure occurred.
	LastFailureTime time.Time `json:"lastFailureTime,omitempty"`
	// LastStateChangeTime is when the state last changed.
	LastStateChangeTime time.Time `json:"lastStateChangeTime"`
}

// ============================================================================
// Standard Circuit Breaker Implementation
// ============================================================================

// StandardCircuitBreaker is the default circuit breaker implementation.
type StandardCircuitBreaker struct {
	config CircuitBreakerConfig

	state            atomic.Int32
	consecutiveSucc  atomic.Int32
	consecutiveFail  atomic.Int32
	totalRequests    atomic.Int64
	totalSuccesses   atomic.Int64
	totalFailures    atomic.Int64
	halfOpenRequests atomic.Int32

	lastFailureTime     time.Time
	lastStateChangeTime time.Time
	openedAt            time.Time

	mu sync.RWMutex

	// Sliding window for failure rate calculation
	slidingWindow *slidingWindow
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(config CircuitBreakerConfig) *StandardCircuitBreaker {
	config = config.WithDefaults()

	cb := &StandardCircuitBreaker{
		config:              config,
		lastStateChangeTime: time.Now(),
	}

	if config.SlidingWindowSize > 0 {
		cb.slidingWindow = newSlidingWindow(config.SlidingWindowSize, config.SlidingWindowDuration)
	}

	return cb
}

// Name returns the circuit breaker name.
func (cb *StandardCircuitBreaker) Name() string {
	return cb.config.Name
}

// State returns the current circuit state.
func (cb *StandardCircuitBreaker) State() CircuitState {
	return CircuitState(cb.state.Load())
}

// Execute runs the function with circuit breaker protection.
func (cb *StandardCircuitBreaker) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := cb.beforeRequest(); err != nil {
		return err
	}

	cb.totalRequests.Add(1)

	err := fn(ctx)

	cb.afterRequest(err)

	return err
}

// beforeRequest checks if the request should be allowed.
func (cb *StandardCircuitBreaker) beforeRequest() error {
	state := cb.State()

	switch state {
	case CircuitClosed:
		return nil

	case CircuitOpen:
		// Check if it's time to transition to half-open
		cb.mu.RLock()
		openedAt := cb.openedAt
		cb.mu.RUnlock()

		if time.Since(openedAt) >= cb.config.OpenTimeout {
			cb.transitionTo(CircuitHalfOpen)
			return cb.beforeRequest() // Re-check in new state
		}
		return ErrCircuitOpen

	case CircuitHalfOpen:
		// Allow limited requests
		current := cb.halfOpenRequests.Add(1)
		if int(current) > cb.config.HalfOpenMaxRequests {
			cb.halfOpenRequests.Add(-1)
			return ErrCircuitHalfOpen
		}
		return nil

	default:
		return nil
	}
}

// afterRequest processes the result.
func (cb *StandardCircuitBreaker) afterRequest(err error) {
	isFailure := err != nil
	if cb.config.IsFailure != nil && err != nil {
		isFailure = cb.config.IsFailure(err)
	}

	state := cb.State()

	if isFailure {
		cb.recordFailure()
	} else {
		cb.recordSuccess()
	}

	// State transitions based on result
	switch state {
	case CircuitClosed:
		if isFailure {
			if cb.shouldOpen() {
				cb.transitionTo(CircuitOpen)
			}
		}

	case CircuitHalfOpen:
		cb.halfOpenRequests.Add(-1)
		if isFailure {
			// Any failure in half-open immediately opens the circuit
			cb.transitionTo(CircuitOpen)
		} else {
			// Check if we have enough successes to close
			if int(cb.consecutiveSucc.Load()) >= cb.config.SuccessThreshold {
				cb.transitionTo(CircuitClosed)
			}
		}
	}
}

// recordSuccess records a successful request.
func (cb *StandardCircuitBreaker) recordSuccess() {
	cb.totalSuccesses.Add(1)
	cb.consecutiveSucc.Add(1)
	cb.consecutiveFail.Store(0)

	if cb.slidingWindow != nil {
		cb.slidingWindow.RecordSuccess()
	}
}

// recordFailure records a failed request.
func (cb *StandardCircuitBreaker) recordFailure() {
	cb.totalFailures.Add(1)
	cb.consecutiveFail.Add(1)
	cb.consecutiveSucc.Store(0)

	cb.mu.Lock()
	cb.lastFailureTime = time.Now()
	cb.mu.Unlock()

	if cb.slidingWindow != nil {
		cb.slidingWindow.RecordFailure()
	}
}

// shouldOpen determines if the circuit should open.
func (cb *StandardCircuitBreaker) shouldOpen() bool {
	if cb.slidingWindow != nil {
		// Use sliding window failure rate
		rate := cb.slidingWindow.FailureRate()
		return rate >= cb.config.FailureRateThreshold
	}

	// Use consecutive failure counting
	return int(cb.consecutiveFail.Load()) >= cb.config.FailureThreshold
}

// transitionTo changes the circuit state.
func (cb *StandardCircuitBreaker) transitionTo(newState CircuitState) {
	oldState := CircuitState(cb.state.Swap(int32(newState)))

	if oldState == newState {
		return
	}

	cb.mu.Lock()
	cb.lastStateChangeTime = time.Now()
	if newState == CircuitOpen {
		cb.openedAt = time.Now()
	}
	cb.mu.Unlock()

	// Reset counters on state change
	if newState == CircuitClosed {
		cb.consecutiveFail.Store(0)
		cb.consecutiveSucc.Store(0)
		cb.halfOpenRequests.Store(0)
	} else if newState == CircuitHalfOpen {
		cb.consecutiveSucc.Store(0)
		cb.halfOpenRequests.Store(0)
	}

	if cb.config.OnStateChange != nil {
		cb.config.OnStateChange(cb.config.Name, oldState, newState)
	}
}

// Reset resets the circuit breaker to closed state.
func (cb *StandardCircuitBreaker) Reset() {
	cb.transitionTo(CircuitClosed)
	cb.consecutiveFail.Store(0)
	cb.consecutiveSucc.Store(0)
	cb.halfOpenRequests.Store(0)
	if cb.slidingWindow != nil {
		cb.slidingWindow.Reset()
	}
}

// Metrics returns circuit breaker metrics.
func (cb *StandardCircuitBreaker) Metrics() CircuitBreakerMetrics {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	return CircuitBreakerMetrics{
		Name:                 cb.config.Name,
		State:                cb.State().String(),
		TotalRequests:        cb.totalRequests.Load(),
		TotalSuccesses:       cb.totalSuccesses.Load(),
		TotalFailures:        cb.totalFailures.Load(),
		ConsecutiveSuccesses: int(cb.consecutiveSucc.Load()),
		ConsecutiveFailures:  int(cb.consecutiveFail.Load()),
		LastFailureTime:      cb.lastFailureTime,
		LastStateChangeTime:  cb.lastStateChangeTime,
	}
}

// Ensure StandardCircuitBreaker implements CircuitBreaker.
var _ CircuitBreaker = (*StandardCircuitBreaker)(nil)

// ============================================================================
// Sliding Window for Failure Rate Calculation
// ============================================================================

type slidingWindow struct {
	mu       sync.Mutex
	size     int
	duration time.Duration
	buckets  []bucket
	head     int
}

type bucket struct {
	successes int
	failures  int
	timestamp time.Time
}

func newSlidingWindow(size int, duration time.Duration) *slidingWindow {
	return &slidingWindow{
		size:     size,
		duration: duration,
		buckets:  make([]bucket, size),
	}
}

func (w *slidingWindow) currentBucket() *bucket {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	bucketDuration := w.duration / time.Duration(w.size)

	// Check if current bucket is still valid
	if now.Sub(w.buckets[w.head].timestamp) < bucketDuration {
		return &w.buckets[w.head]
	}

	// Move to next bucket
	w.head = (w.head + 1) % w.size
	w.buckets[w.head] = bucket{timestamp: now}
	return &w.buckets[w.head]
}

func (w *slidingWindow) RecordSuccess() {
	b := w.currentBucket()
	w.mu.Lock()
	b.successes++
	w.mu.Unlock()
}

func (w *slidingWindow) RecordFailure() {
	b := w.currentBucket()
	w.mu.Lock()
	b.failures++
	w.mu.Unlock()
}

func (w *slidingWindow) FailureRate() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	var totalSuccess, totalFailure int

	for _, b := range w.buckets {
		if now.Sub(b.timestamp) <= w.duration {
			totalSuccess += b.successes
			totalFailure += b.failures
		}
	}

	total := totalSuccess + totalFailure
	if total == 0 {
		return 0
	}

	return float64(totalFailure) / float64(total) * 100
}

func (w *slidingWindow) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buckets = make([]bucket, w.size)
	w.head = 0
}

// ============================================================================
// Circuit Breaker Registry
// ============================================================================

// CircuitBreakerRegistry manages multiple circuit breakers.
type CircuitBreakerRegistry struct {
	mu       sync.RWMutex
	breakers map[string]CircuitBreaker
	config   CircuitBreakerConfig // Default config for new breakers
}

// NewCircuitBreakerRegistry creates a new registry.
func NewCircuitBreakerRegistry(defaultConfig CircuitBreakerConfig) *CircuitBreakerRegistry {
	return &CircuitBreakerRegistry{
		breakers: make(map[string]CircuitBreaker),
		config:   defaultConfig.WithDefaults(),
	}
}

// Get returns a circuit breaker by name, creating one if it doesn't exist.
func (r *CircuitBreakerRegistry) Get(name string) CircuitBreaker {
	r.mu.RLock()
	cb, ok := r.breakers[name]
	r.mu.RUnlock()

	if ok {
		return cb
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if cb, ok := r.breakers[name]; ok {
		return cb
	}

	config := r.config
	config.Name = name
	cb = NewCircuitBreaker(config)
	r.breakers[name] = cb
	return cb
}

// Register adds a circuit breaker with custom config.
func (r *CircuitBreakerRegistry) Register(name string, config CircuitBreakerConfig) CircuitBreaker {
	config.Name = name
	cb := NewCircuitBreaker(config)

	r.mu.Lock()
	r.breakers[name] = cb
	r.mu.Unlock()

	return cb
}

// All returns all circuit breakers.
func (r *CircuitBreakerRegistry) All() map[string]CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		result[k] = v
	}
	return result
}

// Metrics returns metrics for all circuit breakers.
func (r *CircuitBreakerRegistry) Metrics() []CircuitBreakerMetrics {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]CircuitBreakerMetrics, 0, len(r.breakers))
	for _, cb := range r.breakers {
		result = append(result, cb.Metrics())
	}
	return result
}
