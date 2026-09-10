package oteltracing

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
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

	if r := t.config.SamplerRatio; r != nil && (*r < 0 || *r > 1) {
		return nil, fmt.Errorf("otel-tracing: sampler ratio %v out of range [0,1]", *r)
	}

	client, err := newTracerClient(ctx, t.config)
	if err != nil {
		return nil, err
	}
	t.client = client

	return t, nil
}

// StartSpan begins a new span with the given name and optional
// shared.Tag attributes. The returned context carries the active span.
func (t *Tracer) StartSpan(
	ctx context.Context,
	name string,
	tags ...shared.Tag,
) (context.Context, ports.Span) {
	if t.client == nil {
		return ctx, noopSpan{}
	}
	return t.client.StartSpan(ctx, name, tags)
}

// Extract reads a W3C trace context (traceparent/tracestate) from the
// given carrier headers into ctx, so a span started next becomes a
// child of the remote parent. Headers use the bridge's map[string]any
// bag; the method is SDK-free at its boundary.
func (t *Tracer) Extract(ctx context.Context, headers map[string]any) context.Context {
	if t.client == nil {
		return ctx
	}
	return t.client.Extract(ctx, headers)
}

// Inject writes the active span context in ctx onto the carrier headers
// for outbound W3C propagation, returning the (possibly new) map.
func (t *Tracer) Inject(ctx context.Context, headers map[string]any) map[string]any {
	if t.client == nil {
		if headers == nil {
			return make(map[string]any)
		}
		return headers
	}
	return t.client.Inject(ctx, headers)
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
func (noopSpan) AddEvent(string, ...shared.Tag) {}
func (noopSpan) SetAttributes(...shared.Tag)    {}
