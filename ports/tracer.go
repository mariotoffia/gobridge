package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Span represents an active trace span. Implementations must be safe
// for use from the goroutine that created the span.
type Span interface {
	End()
	SetError(err error)
	AddEvent(name string, attrs ...shared.Tag)
	SetAttributes(attrs ...shared.Tag)
}

// SpanIdentity is an OPTIONAL Span capability. A Span that knows its
// own W3C trace-context identity implements it so the runtime can stamp
// the trace_id/span_id log-correlation fields from the ACTIVE span
// (observability.WithTraceID/WithSpanID) instead of the upstream
// traceparent header: logs then carry THIS hop's span id, and root
// deliveries — which have no upstream traceparent — get a trace_id at
// all. The runtime probes the capability with a type assertion and
// falls back to the upstream traceparent when it is absent (NoopTracer)
// or when both accessors return "".
//
// TraceID returns the 32-hex-digit lowercase W3C trace-id and SpanID
// the 16-hex-digit lowercase W3C span-id of the active span. Both
// return "" when the identity is unavailable (no-op span, invalid span
// context) or the trace is unsampled — an unsampled trace exports no
// spans, so a logged trace_id would dangle; the correlation ID
// (UBIQUITOUS.md "Correlation ID") remains the always-present join key.
type SpanIdentity interface {
	TraceID() string
	SpanID() string
}

// Tracer creates spans for distributed tracing. Implementations must
// be safe for concurrent use from multiple goroutines.
type Tracer interface {
	StartSpan(ctx context.Context, name string, attrs ...shared.Tag) (context.Context, Span)
	// Extract reads W3C trace context (traceparent/tracestate) from the
	// carrier headers into ctx, so a span started next joins the remote
	// trace as a child of the upstream span.
	Extract(ctx context.Context, headers map[string]any) context.Context
	// Inject writes the active span context in ctx onto the carrier
	// headers for downstream W3C propagation, returning the (possibly
	// newly allocated) header map. Other headers are preserved.
	Inject(ctx context.Context, headers map[string]any) map[string]any
	// Close flushes buffered spans and releases the tracer's resources,
	// bounded by ctx. It is called once during runtime shutdown.
	Close(ctx context.Context) error
}

// NoopTracer is a Tracer that produces no-op spans.
// It is the default when no tracer is configured.
type NoopTracer struct{}

var _ Tracer = (*NoopTracer)(nil)

func (NoopTracer) StartSpan(ctx context.Context, _ string, _ ...shared.Tag) (context.Context, Span) {
	return ctx, noopSpan{}
}

// Extract is a no-op: without a real propagator the context is unchanged.
func (NoopTracer) Extract(ctx context.Context, _ map[string]any) context.Context {
	return ctx
}

// Inject is a no-op that returns the headers unchanged (allocating an
// empty map only when given nil), so callers can uniformly assign the
// result whether or not tracing is enabled.
func (NoopTracer) Inject(_ context.Context, headers map[string]any) map[string]any {
	if headers == nil {
		return make(map[string]any)
	}
	return headers
}

// Close is a no-op; the no-op tracer holds no resources.
func (NoopTracer) Close(context.Context) error { return nil }

type noopSpan struct{}

var _ Span = noopSpan{}

func (noopSpan) End()                           {}
func (noopSpan) SetError(error)                 {}
func (noopSpan) AddEvent(string, ...shared.Tag) {}
func (noopSpan) SetAttributes(...shared.Tag)    {}
