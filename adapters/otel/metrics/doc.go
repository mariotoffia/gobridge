// Package otelmetrics provides a ports.MetricsExporter implementation
// that exports metrics via OpenTelemetry using the OTLP HTTP protocol.
//
// Instruments (counters, gauges, histograms) are created lazily with
// double-checked locking so that metric names do not need to be
// registered up front.
//
// # Metric-name cardinality
//
// Emit metric names from a bounded, static set (see
// domain/shared/metrics.go). The instrument cache is bounded by
// WithMaxInstruments (default 1024); once full, a new (dynamic) name is
// rejected and surfaced via the error handler instead of growing the
// cache without bound.
//
// # Export-failure visibility
//
// WithErrorHandler installs a callback invoked when a metric export
// fails or an instrument is rejected. WithExportTimeout bounds a single
// periodic export and WithFlushInterval sets the export cadence.
//
// # Environment variables
//
// When the corresponding option is not set, the adapter honors the
// standard OpenTelemetry environment variables (option > env > default):
//   - OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
//   - OTEL_EXPORTER_OTLP_HEADERS
//   - OTEL_SERVICE_NAME
//   - OTEL_RESOURCE_ATTRIBUTES
//
// # Lifecycle
//
// Flush forces an export; Close shuts down the MeterProvider within the
// supplied context, flushing remaining metrics.
package otelmetrics
