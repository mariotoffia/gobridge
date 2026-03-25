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

type Config struct {
	FailureThreshold int
	SuccessThreshold int
	ResetTimeout     time.Duration
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
