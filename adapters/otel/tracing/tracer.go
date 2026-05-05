package oteltracing

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Tracer = (*Tracer)(nil)

// Tracer implements [ports.Tracer] by delegating to an OpenTelemetry
// TracerProvider configured with an OTLP HTTP exporter. The SDK
// boundary is encapsulated in the unexported tracerClient seam
// declared in acl_client.go; this file is SDK-import-free.
type Tracer struct {
	config Config
	client tracerClient
}

// New creates a Tracer that exports spans to an OTLP-compatible
// collector over HTTP. The returned Tracer must be closed with Close
// to flush pending spans and release resources.
func New(ctx context.Context, opts ...Option) (*Tracer, error) {
	t := &Tracer{}
	for _, o := range opts {
		o(t)
	}
	applyDefaults(&t.config)

	client, err := newTracerClient(ctx, t.config)
	if err != nil {
		return nil, err
	}
	t.client = client

	return t, nil
}

// StartSpan begins a new span with the given name and optional
// domain.Tag attributes. The returned context carries the active span.
func (t *Tracer) StartSpan(
	ctx context.Context,
	name string,
	tags ...domain.Tag,
) (context.Context, ports.Span) {
	if t.client == nil {
		return ctx, noopSpan{}
	}
	return t.client.StartSpan(ctx, name, tags)
}

// Close shuts down the TracerProvider, flushing any pending spans.
func (t *Tracer) Close(ctx context.Context) error {
	if t.client == nil {
		return nil
	}
	return t.client.Close(ctx)
}

// noopSpan is returned when StartSpan is invoked on a Tracer that has
// no client configured (e.g. NewForTest without a provider).
type noopSpan struct{}

func (noopSpan) End()                           {}
func (noopSpan) SetError(error)                 {}
func (noopSpan) AddEvent(string, ...domain.Tag) {}
func (noopSpan) SetAttributes(...domain.Tag)    {}
