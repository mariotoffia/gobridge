package oteltracing

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ ports.Tracer = (*Tracer)(nil)
	_ ports.Span   = (*otelSpan)(nil)
)

// Tracer implements ports.Tracer by delegating to an OpenTelemetry
// TracerProvider configured with an OTLP HTTP exporter.
type Tracer struct {
	config   Config
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
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

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(t.config.Endpoint),
	}
	if t.config.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	if len(t.config.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracehttp.WithHeaders(t.config.Headers))
	}

	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel-tracing: create otlp exporter: %w", err)
	}

	resAttrs := []attribute.KeyValue{
		semconv.ServiceName(t.config.ServiceName),
	}
	if t.config.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersion(t.config.ServiceVersion))
	}
	if t.config.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironment(t.config.Environment))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("otel-tracing: create resource: %w", err)
	}

	t.provider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(t.config.SamplerRatio)),
	)
	t.tracer = t.provider.Tracer("github.com/mariotoffia/gobridge/adapters/otel/tracing")

	return t, nil
}

// StartSpan begins a new span with the given name and optional
// domain.Tag attributes. The returned context carries the active span.
func (t *Tracer) StartSpan(
	ctx context.Context,
	name string,
	tags ...domain.Tag,
) (context.Context, ports.Span) {
	var spanOpts []trace.SpanStartOption
	if len(tags) > 0 {
		spanOpts = append(spanOpts, trace.WithAttributes(tagsToAttributes(tags)...))
	}

	ctx, span := t.tracer.Start(ctx, name, spanOpts...)
	return ctx, &otelSpan{span: span}
}

// Close shuts down the TracerProvider, flushing any pending spans.
func (t *Tracer) Close(ctx context.Context) error {
	if err := t.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("otel-tracing: shutdown: %w", err)
	}
	return nil
}

// otelSpan wraps an OTel trace.Span and implements ports.Span.
type otelSpan struct {
	span trace.Span
}

func (s *otelSpan) End() {
	s.span.End()
}

func (s *otelSpan) SetError(err error) {
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
}

func (s *otelSpan) AddEvent(name string, attrs ...domain.Tag) {
	if len(attrs) > 0 {
		s.span.AddEvent(name, trace.WithAttributes(tagsToAttributes(attrs)...))
		return
	}
	s.span.AddEvent(name)
}

func (s *otelSpan) SetAttributes(attrs ...domain.Tag) {
	if len(attrs) > 0 {
		s.span.SetAttributes(tagsToAttributes(attrs)...)
	}
}

func tagsToAttributes(tags []domain.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(tags))
	for i, t := range tags {
		attrs[i] = attribute.String(t.Key, t.Value)
	}
	return attrs
}
