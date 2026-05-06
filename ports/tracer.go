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
}

// NoopTracer is a Tracer that produces no-op spans.
// It is the default when no tracer is configured.
type NoopTracer struct{}

var _ Tracer = (*NoopTracer)(nil)

func (NoopTracer) StartSpan(ctx context.Context, _ string, _ ...shared.Tag) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

var _ Span = noopSpan{}

func (noopSpan) End()                           {}
func (noopSpan) SetError(error)                 {}
func (noopSpan) AddEvent(string, ...shared.Tag) {}
func (noopSpan) SetAttributes(...shared.Tag)    {}
