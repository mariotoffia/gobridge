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
// breaker failure thresholds. When nil, the default classifier counts
// transient (recoverable) errors EXCEPT tenant in-flight quota rejects
// (permanent/rejected errors are ignored so malformed input cannot trip the
// breaker on a healthy dependency; a quota reject is a per-tenant fairness
// signal, not downstream ill-health). See defaultCountError.
type ErrorClassifier func(error) bool

// Config holds circuit breaker parameters.
//
// The yaml/json tags define the serialized key names so a configuration
// surface (file, blueprint, HTTP API) can decode directly into this
// struct. CountError is a function and is never serialized; a decoding
// caller gets the default classifier via WithDefaults.
type Config struct {
	FailureThreshold int           `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`
	SuccessThreshold int           `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
	ResetTimeout     time.Duration `json:"resetTimeout,omitempty" yaml:"resetTimeout,omitempty"`
	// HalfOpenMaxProbes limits concurrent requests allowed in half-open
	// state. Defaults to 1 to prevent thundering herd on a recovering
	// dependency.
	HalfOpenMaxProbes int `json:"halfOpenMaxProbes,omitempty" yaml:"halfOpenMaxProbes,omitempty"`
	// CountError determines whether an error counts toward the failure
	// threshold. Defaults to counting only transient errors.
	CountError ErrorClassifier `json:"-" yaml:"-"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
	}
}

// WithDefaults returns a copy of c with zero or invalid (negative)
// fields replaced by defaults. A negative threshold or timeout is a
// misconfiguration, not a policy: FailureThreshold:-1 would otherwise
// open the circuit on the first failure, so negatives are normalised to
// the same defaults as the zero value.
func (c Config) WithDefaults() Config {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.ResetTimeout <= 0 {
		c.ResetTimeout = 30 * time.Second
	}
	if c.HalfOpenMaxProbes <= 0 {
		c.HalfOpenMaxProbes = 1
	}
	if c.CountError == nil {
		c.CountError = defaultCountError
	}
	return c
}

// defaultCountError counts an error toward tripping the breaker when it is
// recoverable (transient) — EXCEPT a tenant in-flight quota reject
// (ErrCodeTenantQuotaExceeded). A quota reject is a per-tenant fairness signal,
// not a downstream-health signal; counting it lets one throttled tenant trip a
// shared (e.g. GlobalKey) breaker and deny every other tenant.
//
// Note: a NESTED breaker being open returns shared.ErrUnavailable
// (ErrCodeUnavailable), which is INDISTINGUISHABLE from a genuine
// downstream-unavailable failure and is therefore still counted — if you stack
// breakers, make the outer breaker the aggregate (or override CountError) so an
// inner-open error does not cascade into tripping the outer breaker.
func defaultCountError(err error) bool {
	if be, ok := shared.AsBridgeError(err); ok && be.Code == shared.ErrCodeTenantQuotaExceeded {
		return false
	}
	return shared.IsRecoverableError(err)
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
