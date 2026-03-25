package types

import (
	"context"
)

// ============================================================================
// Observability Interfaces
// ============================================================================

// Tracer provides tracing capabilities.
// This is a generic interface that can be implemented by OpenTelemetry or other tracing systems.
type Tracer interface {
	// StartSpan starts a new span with the given name.
	// Returns a context containing the span and a function to end the span.
	StartSpan(ctx context.Context, name string, opts ...SpanOption) (context.Context, SpanEnder)
}

// SpanEnder ends a span.
type SpanEnder interface {
	// End ends the span with optional error.
	End(err error)
}

// SpanOption configures a span.
type SpanOption func(*SpanConfig)

// SpanConfig holds span configuration.
type SpanConfig struct {
	// Attributes to add to the span.
	Attributes map[string]any
	// Kind of span (client, server, producer, consumer, internal).
	Kind string
}

// WithSpanAttribute adds an attribute to the span.
func WithSpanAttribute(key string, value any) SpanOption {
	return func(c *SpanConfig) {
		if c.Attributes == nil {
			c.Attributes = make(map[string]any)
		}
		c.Attributes[key] = value
	}
}

// WithSpanKind sets the span kind.
func WithSpanKind(kind string) SpanOption {
	return func(c *SpanConfig) {
		c.Kind = kind
	}
}

// Meter provides metrics capabilities.
// This is a generic interface that can be implemented by OpenTelemetry or other metrics systems.
type Meter interface {
	// Counter creates or gets a counter metric.
	Counter(name string, description string) Counter
	// Histogram creates or gets a histogram metric.
	Histogram(name string, description string) Histogram
	// Gauge creates or gets a gauge metric.
	Gauge(name string, description string) Gauge
}

// Counter is a monotonically increasing metric.
type Counter interface {
	// Add increments the counter by the given value.
	Add(ctx context.Context, value int64, attrs ...Attribute)
}

// Histogram records a distribution of values.
type Histogram interface {
	// Record records a value in the histogram.
	Record(ctx context.Context, value float64, attrs ...Attribute)
}

// Gauge records a value that can go up or down.
type Gauge interface {
	// Set sets the gauge to the given value.
	Set(ctx context.Context, value float64, attrs ...Attribute)
}

// Attribute is a key-value pair for metrics labels.
type Attribute struct {
	Key   string
	Value any
}

// Attr creates an attribute.
func Attr(key string, value any) Attribute {
	return Attribute{Key: key, Value: value}
}

// ============================================================================
// No-op Implementations
// ============================================================================

// NoopTracer is a no-op implementation of Tracer.
type NoopTracer struct{}

var _ Tracer = (*NoopTracer)(nil)

func (n *NoopTracer) StartSpan(ctx context.Context, name string, opts ...SpanOption) (context.Context, SpanEnder) {
	return ctx, &noopSpanEnder{}
}

type noopSpanEnder struct{}

func (n *noopSpanEnder) End(err error) {}

// NoopMeter is a no-op implementation of Meter.
type NoopMeter struct{}

var _ Meter = (*NoopMeter)(nil)

func (n *NoopMeter) Counter(name string, description string) Counter     { return &noopCounter{} }
func (n *NoopMeter) Histogram(name string, description string) Histogram { return &noopHistogram{} }
func (n *NoopMeter) Gauge(name string, description string) Gauge         { return &noopGauge{} }

type noopCounter struct{}

func (n *noopCounter) Add(ctx context.Context, value int64, attrs ...Attribute) {}

type noopHistogram struct{}

func (n *noopHistogram) Record(ctx context.Context, value float64, attrs ...Attribute) {}

type noopGauge struct{}

func (n *noopGauge) Set(ctx context.Context, value float64, attrs ...Attribute) {}
