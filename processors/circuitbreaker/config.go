package circuitbreaker

import (
	"context"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// KeyExtractor derives a circuit breaker key from each envelope.
type KeyExtractor func(ctx context.Context, env *messaging.Envelope) string

// GlobalKey returns a constant key so all envelopes share one breaker.
func GlobalKey(_ context.Context, _ *messaging.Envelope) string {
	return "global"
}

// SubjectKey uses the envelope's Subject as the breaker key.
func SubjectKey(_ context.Context, env *messaging.Envelope) string {
	return env.Subject()
}

// HeaderKey returns a KeyExtractor that reads a named header.
//
// Caution: the breaker cache is bounded (see WithMaxBreakers). Keying on a
// caller-controlled header creates one breaker per distinct value, so an
// untrusted or unbounded header can churn the cache and, at the extreme,
// evict open breakers. Only key on headers whose value space you trust and
// bound.
func HeaderKey(name string) KeyExtractor {
	return func(_ context.Context, env *messaging.Envelope) string {
		if v, ok := messaging.GetHeaderString(env.Headers(), name); ok {
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

// WithMaxBreakers overrides the maximum number of per-key breakers the
// Processor caches (default 10000). When the cache is full the
// least-recently-failed closed breaker is evicted first and open breakers are
// dropped only as a last resort (surfaced via Stats().OpenEvictions). A
// non-positive value keeps the default. Size this to your trusted key
// cardinality.
func WithMaxBreakers(n int) Option {
	return func(p *Processor) {
		if n > 0 {
			p.maxBreakers = n
		}
	}
}

// WithClock injects the clock used by every breaker this Processor creates,
// so tests can drive Open->HalfOpen transitions with a fake clock instead of
// sleeping for the reset timeout. A nil clock keeps the system clock.
func WithClock(clk clock.Clock) Option {
	return func(p *Processor) {
		if clk != nil {
			p.clk = clk
		}
	}
}

// WithOnStateChange registers a callback for state transitions.
func WithOnStateChange(fn func(key string, from, to cb.State)) Option {
	return func(p *Processor) {
		p.onStateChange = fn
	}
}
