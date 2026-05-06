package oteltracing

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerClient is the adapter-internal mock seam shielding the
// port-side Tracer from OpenTelemetry SDK types. The TracerProvider,
// the tracer instance and the SDK Span construction all live behind
// this interface.
type tracerClient interface {
	StartSpan(ctx context.Context, name string, tags []shared.Tag) (context.Context, ports.Span)
	Close(ctx context.Context) error
}

// otelTracerClient is the production tracerClient backed by an
// OpenTelemetry TracerProvider plus a derived Tracer.
type otelTracerClient struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

// newTracerClient constructs a production tracerClient backed by an
// OTLP HTTP exporter and a batching TracerProvider configured per cfg.
func newTracerClient(ctx context.Context, cfg Config) (*otelTracerClient, error) {
	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracehttp.WithHeaders(cfg.Headers))
	}

	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel-tracing: create otlp exporter: %w", err)
	}

	resAttrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironment(cfg.Environment))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("otel-tracing: create resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SamplerRatio)),
	)

	return newTracerClientFromProvider(provider), nil
}

// newTracerClientFromProvider wraps an existing TracerProvider in a
// tracerClient. Used by tests that drive the adapter via an
// in-memory span recorder without a network exporter.
func newTracerClientFromProvider(tp *sdktrace.TracerProvider) *otelTracerClient {
	return &otelTracerClient{
		provider: tp,
		tracer:   tp.Tracer("github.com/mariotoffia/gobridge/adapters/otel/tracing"),
	}
}

func (c *otelTracerClient) StartSpan(
	ctx context.Context,
	name string,
	tags []shared.Tag,
) (context.Context, ports.Span) {
	var spanOpts []trace.SpanStartOption
	if len(tags) > 0 {
		spanOpts = append(spanOpts, trace.WithAttributes(tagsToAttributes(tags)...))
	}

	ctx, span := c.tracer.Start(ctx, name, spanOpts...)
	return ctx, &otelSpan{span: span}
}

func (c *otelTracerClient) Close(ctx context.Context) error {
	if err := c.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("otel-tracing: shutdown: %w", err)
	}
	return nil
}

func tagsToAttributes(tags []shared.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(tags))
	for i, t := range tags {
		attrs[i] = attribute.String(t.Key, t.Value)
	}
	return attrs
}
