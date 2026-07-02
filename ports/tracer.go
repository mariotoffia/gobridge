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
