package circuitbreaker

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

type KeyExtractor func(ctx context.Context, env *domain.Envelope) string

func GlobalKey(_ context.Context, _ *domain.Envelope) string {
	return "global"
}

func SubjectKey(_ context.Context, env *domain.Envelope) string {
	return env.Subject
}

func HeaderKey(name string) KeyExtractor {
	return func(_ context.Context, env *domain.Envelope) string {
		if v, ok := domain.GetHeaderString(env.Headers, name); ok {
			return v
		}
		return "unknown"
	}
}

// ErrorClassifier returns true if the error should count toward circuit
// breaker failure thresholds. When nil, only transient errors are counted
// (permanent/rejected errors are ignored to prevent malformed input from
// tripping the breaker on a healthy dependency).
type ErrorClassifier func(error) bool

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

func DefaultConfig() Config {
	return Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
	}
}

func (c Config) withDefaults() Config {
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
		c.CountError = domain.IsRecoverableError
	}
	return c
}

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

type Option func(*Processor)

func WithKeyExtractor(ke KeyExtractor) Option {
	return func(p *Processor) {
		p.keyExtractor = ke
	}
}

func WithOnStateChange(fn func(key string, from, to State)) Option {
	return func(p *Processor) {
		p.onStateChange = fn
	}
}
