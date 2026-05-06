// Package oteltracing provides a ports.Tracer implementation that
// delegates to OpenTelemetry using the OTLP HTTP trace exporter.
//
// Spans are created via the standard OTel SDK and exported to an
// OTLP-compatible collector. shared.Tag values are mapped to
// attribute.String key-value pairs on each span.
package oteltracing
