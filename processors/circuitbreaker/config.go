package circuitbreaker

import (
	"context"
	"log/slog"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
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

// WithMaxBreakers overrides the target size of the per-key breaker cache
// (default 10000). It is a SOFT cap: when the cache is full the
// least-recently-failed closed breaker is evicted first and open breakers are
// dropped only as a last resort (surfaced via Stats().OpenEvictions), but if
// every eviction candidate is in-flight the new breaker is inserted anyway and
// the cache temporarily overshoots (see Stats). A non-positive value keeps the
// default. Size this to your trusted key cardinality plus concurrency headroom.
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

// WithOnStateChange registers a callback for state transitions, replacing
// the default handler (which logs every transition at Warn via the
// processor's logger — see WithLogger). Passing nil keeps the default.
func WithOnStateChange(fn func(key string, from, to cb.State)) Option {
	return func(p *Processor) {
		if fn != nil {
			p.onStateChange = fn
		}
	}
}

// WithLogger sets the structured logger used by the default state-change
// handler. A nil logger keeps slog.Default(). Ignored for transitions when
// WithOnStateChange replaces the default handler.
func WithLogger(l *slog.Logger) Option {
	return func(p *Processor) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithMetrics sets the metrics exporter used to emit the shared
// circuit-breaker counters (shared.MetricCircuitBreakerStateChanged,
// ...Trips, ...Rejections). State-change metrics are emitted for every
// transition regardless of WithOnStateChange. When unset (or nil), a
// NoopExporter is used.
func WithMetrics(m ports.MetricsExporter) Option {
	return func(p *Processor) {
		if m != nil {
			p.metrics = m
		}
	}
}

// WithMetricKeyCardinality bounds the number of DISTINCT breaker-key values
// emitted as the "key" dimension on the shared circuit-breaker metrics
// (CircuitBreakerStateChanged / Trips / Rejections). The first n distinct keys
// are tagged verbatim; every further distinct key collapses to the "other"
// bucket, capping the dimension at n+1 series no matter how many unique keys a
// (possibly caller-controlled) KeyExtractor produces. This prevents a metrics
// cardinality DoS when keying on an untrusted header — see HeaderKey — where
// one producer sending unique values could otherwise create an unbounded
// number of metric series. It is independent of WithMaxBreakers, which bounds
// the breaker CACHE (memory), not the number of metric series. A non-positive
// n keeps the default (256).
func WithMetricKeyCardinality(n int) Option {
	return func(p *Processor) {
		if n > 0 {
			p.metricKeyLimit = n
		}
	}
}
