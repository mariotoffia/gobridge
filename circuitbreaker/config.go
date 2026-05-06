// Package circuitbreaker provides a standalone circuit breaker state machine.
// It is used by processors/circuitbreaker for middleware-level circuit breaking
// and by transport adapters for connection-level circuit breaking.
//
// The package lives in the root module so both inner layers (runtime, processors)
// and outer layers (adapters) can depend on it without introducing cross-layer
// coupling between processors and adapters.
package circuitbreaker

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// ErrorClassifier returns true if the error should count toward circuit
// breaker failure thresholds. When nil, only transient errors are counted
// (permanent/rejected errors are ignored to prevent malformed input from
// tripping the breaker on a healthy dependency).
type ErrorClassifier func(error) bool

// Config holds circuit breaker parameters.
type Config struct {
	FailureThreshold int
	SuccessThreshold int
	ResetTimeout     time.Duration
	// HalfOpenMaxProbes limits concurrent requests allowed in half-open
	// state. Defaults to 1 to prevent thundering herd on a recovering
	// dependency.
	HalfOpenMaxProbes int
	// CountError determines whether an error counts toward the failure
	// threshold. Defaults to counting only transient errors.
	CountError ErrorClassifier
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
	}
}

// WithDefaults returns a copy of c with zero fields replaced by defaults.
func (c Config) WithDefaults() Config {
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold == 0 {
		c.SuccessThreshold = 2
	}
	if c.ResetTimeout == 0 {
		c.ResetTimeout = 30 * time.Second
	}
	if c.HalfOpenMaxProbes <= 0 {
		c.HalfOpenMaxProbes = 1
	}
	if c.CountError == nil {
		c.CountError = shared.IsRecoverableError
	}
	return c
}

// State represents the circuit breaker state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}
