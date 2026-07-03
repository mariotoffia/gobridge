package oteltracing

import (
	"context"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerClient is the adapter-internal mock seam shielding the
// port-side Tracer from OpenTelemetry SDK types. The TracerProvider,
// the tracer instance, W3C context propagation and the SDK Span
// construction all live behind this interface.
type tracerClient interface {
	StartSpan(ctx context.Context, name string, tags []shared.Tag) (context.Context, ports.Span)
	// Extract reads a W3C trace context from carrier headers into ctx
	// so a subsequently started span becomes a child of the remote
	// parent (K1). Signature is SDK-free by design.
	Extract(ctx context.Context, headers map[string]any) context.Context
	// Inject writes the active span context in ctx onto carrier headers
	// for outbound propagation (K1). Returns the (possibly new) map.
	Inject(ctx context.Context, headers map[string]any) map[string]any
	Close(ctx context.Context) error
}

// otelTracerClient is the production tracerClient backed by an
// OpenTelemetry TracerProvider plus a derived Tracer and a W3C
// TraceContext propagator.
type otelTracerClient struct {
	provider   *sdktrace.TracerProvider
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// newTracerClient constructs a production tracerClient backed by an
// OTLP HTTP exporter and a batching TracerProvider configured per cfg.
func newTracerClient(ctx context.Context, cfg Config) (*otelTracerClient, error) {
	var exporterOpts []otlptracehttp.Option
	// Only pin the endpoint when explicitly configured; otherwise the
	// SDK honors OTEL_EXPORTER_OTLP[_TRACES]_ENDPOINT env vars (K7).
	if cfg.Endpoint != "" {
		exporterOpts = append(exporterOpts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
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

	// resource.Default() already merges OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES; only override attributes explicitly set
	// via options so env-provided values are not clobbered (K7).
	var resAttrs []attribute.KeyValue
	if cfg.ServiceName != "" {
		resAttrs = append(resAttrs, semconv.ServiceName(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironment(cfg.Environment))
	}

	// NewSchemaless avoids a schema-URL conflict with resource.Default(),
	// whose semconv version may differ from ours across SDK bumps.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("otel-tracing: create resource: %w", err)
	}

	observed := &observedSpanExporter{SpanExporter: exporter, onError: cfg.errorHandler}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(observed, batchOptions(cfg)...),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(samplerFromConfig(cfg)),
	)

	return newTracerClientFromProvider(provider), nil
}

// batchOptions maps configured batch/queue tuning onto SDK options,
// keeping SDK defaults for any zero value.
func batchOptions(cfg Config) []sdktrace.BatchSpanProcessorOption {
	var opts []sdktrace.BatchSpanProcessorOption
	if cfg.BatchTimeout > 0 {
		opts = append(opts, sdktrace.WithBatchTimeout(cfg.BatchTimeout))
	}
	if cfg.ExportTimeout > 0 {
		opts = append(opts, sdktrace.WithExportTimeout(cfg.ExportTimeout))
	}
	if cfg.MaxQueueSize > 0 {
		opts = append(opts, sdktrace.WithMaxQueueSize(cfg.MaxQueueSize))
	}
	if cfg.MaxExportBatchSize > 0 {
		opts = append(opts, sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize))
	}
	return opts
}

// samplerFromConfig builds a ParentBased sampler whose root decision is
// a TraceIDRatioBased sampler at the configured ratio. ParentBased
// respects a sampled remote parent (propagation), while a ratio of 0.0
// disables sampling of new root traces (K6). SamplerRatio is assumed
// non-nil (applyDefaults guarantees it).
func samplerFromConfig(cfg Config) sdktrace.Sampler {
	ratio := 1.0
	if cfg.SamplerRatio != nil {
		ratio = *cfg.SamplerRatio
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// newTracerClientFromProvider wraps an existing TracerProvider in a
// tracerClient. Used by tests that drive the adapter via an
// in-memory span recorder without a network exporter.
func newTracerClientFromProvider(tp *sdktrace.TracerProvider) *otelTracerClient {
	return &otelTracerClient{
		provider:   tp,
		tracer:     tp.Tracer("github.com/mariotoffia/gobridge/adapters/otel/tracing"),
		propagator: propagation.TraceContext{},
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

	// ctx may already carry a remote span context established by
	// Extract; tracer.Start then parents the new span accordingly (K1).
	ctx, span := c.tracer.Start(ctx, name, spanOpts...)
	return ctx, &otelSpan{span: span}
}

func (c *otelTracerClient) Extract(ctx context.Context, headers map[string]any) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	return c.propagator.Extract(ctx, headerCarrier(headers))
}

func (c *otelTracerClient) Inject(ctx context.Context, headers map[string]any) map[string]any {
	if headers == nil {
		headers = make(map[string]any)
	}
	c.propagator.Inject(ctx, headerCarrier(headers))
	return headers
}

func (c *otelTracerClient) Close(ctx context.Context) error {
	if err := c.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("otel-tracing: shutdown: %w", err)
	}
	return nil
}

// headerCarrier adapts the bridge's map[string]any header bag to the
// OTel propagation.TextMapCarrier interface. W3C keys ("traceparent",
// "tracestate") are written lowercase, matching the bridge header
// vocabulary, but lookup is case-insensitive per the W3C Trace Context
// spec (MF-9): transports that stamp "Traceparent" (HTTP-style header
// casing) must not silently break trace continuity.
type headerCarrier map[string]any

var _ propagation.TextMapCarrier = headerCarrier(nil)

func (c headerCarrier) Get(key string) string {
	// Fast path: exact match (bridge vocabulary is lowercase).
	if s, ok := c[key].(string); ok {
		return s
	}
	// Slow path: case-insensitive scan per W3C header semantics.
	for k, v := range c {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func (c headerCarrier) Set(key, value string) {
	c[key] = value
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// observedSpanExporter wraps a SpanExporter to surface export failures
// through an error callback that would otherwise be invisible (K3).
type observedSpanExporter struct {
	sdktrace.SpanExporter
	onError func(error)
}

func (e *observedSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err != nil && e.onError != nil {
		e.onError(fmt.Errorf("otel-tracing: export spans: %w", err))
	}
	return err //nolint:wrapcheck // decorator pass-through: onError already wraps for observability; the sdktrace.SpanExporter contract requires returning the SDK error verbatim.
}

func tagsToAttributes(tags []shared.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(tags))
	for i, t := range tags {
		attrs[i] = attribute.String(t.Key, t.Value)
	}
	return attrs
}
