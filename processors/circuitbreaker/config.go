package circuitbreaker

import (
	"context"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain"
)

// KeyExtractor derives a circuit breaker key from each envelope.
type KeyExtractor func(ctx context.Context, env *domain.Envelope) string

// GlobalKey returns a constant key so all envelopes share one breaker.
func GlobalKey(_ context.Context, _ *domain.Envelope) string {
	return "global"
}

// SubjectKey uses the envelope's Subject as the breaker key.
func SubjectKey(_ context.Context, env *domain.Envelope) string {
	return env.Subject
}

// HeaderKey returns a KeyExtractor that reads a named header.
func HeaderKey(name string) KeyExtractor {
	return func(_ context.Context, env *domain.Envelope) string {
		if v, ok := domain.GetHeaderString(env.Headers, name); ok {
			return v
		}
		return "unknown"
	}
}

// Option configures the Processor.
type Option func(*Processor)

// WithKeyExtractor sets the key extraction strategy.
func WithKeyExtractor(ke KeyExtractor) Option {
	return func(p *Processor) {
		p.keyExtractor = ke
	}
}

// WithOnStateChange registers a callback for state transitions.
func WithOnStateChange(fn func(key string, from, to cb.State)) Option {
	return func(p *Processor) {
		p.onStateChange = fn
	}
}
