// Package oteltracing provides a ports.Tracer implementation that
// delegates to OpenTelemetry using the OTLP HTTP trace exporter.
//
// Spans are created via the standard OTel SDK and exported to an
// OTLP-compatible collector. shared.Tag values are mapped to
// attribute.String key-value pairs on each span.
//
// # Context propagation (W3C Trace Context)
//
// Extract reads a traceparent/tracestate carrier into the Go context so
// the next StartSpan is parented on the remote span; Inject writes the
// active span context back onto an outbound carrier. Both use the OTel
// propagation.TraceContext propagator and take the bridge's
// map[string]any header bag, keeping the boundary SDK-free.
//
// # Sampling
//
// WithSamplerRatio sets the head-sampling ratio in [0,1]. A ratio of
// 0.0 disables sampling of new (root) traces; the sampler is
// ParentBased so a sampled remote parent is still honored. New returns
// an error for ratios outside [0,1].
//
// # Export-failure visibility
//
// WithErrorHandler installs a callback invoked when a span export
// fails. When never configured, failures are logged at Warn level via
// slog.Default(); WithErrorHandler(nil) suppresses reporting.
// WithBatchTimeout / WithExportTimeout / WithMaxQueueSize /
// WithMaxExportBatchSize tune the BatchSpanProcessor.
//
// # Environment variables
//
// When the corresponding option is not set, the adapter honors the
// standard OpenTelemetry environment variables (option > env > default):
//   - OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
//   - OTEL_EXPORTER_OTLP_HEADERS
//   - OTEL_SERVICE_NAME
//   - OTEL_RESOURCE_ATTRIBUTES
//
// # Lifecycle
//
// Close shuts down the TracerProvider within the supplied context,
// flushing buffered spans; it must be called during shutdown to avoid
// losing tail spans.
package oteltracing
