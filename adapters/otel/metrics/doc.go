// Package otelmetrics provides a ports.MetricsExporter implementation
// that exports metrics via OpenTelemetry using the OTLP HTTP protocol.
//
// Instruments (counters, gauges, histograms) are created lazily with
// double-checked locking so that metric names do not need to be
// registered up front.
package otelmetrics
