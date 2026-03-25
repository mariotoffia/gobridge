package otelmetrics

import (
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// NewForTest creates an Exporter with options applied and defaults set,
// without establishing a network connection to a collector.
func NewForTest(opts ...Option) *Exporter {
	e := &Exporter{
		counters:   make(map[string]metric.Int64Counter),
		gauges:     make(map[string]metric.Float64Gauge),
		histograms: make(map[string]metric.Float64Histogram),
	}
	for _, o := range opts {
		o(e)
	}
	applyDefaults(&e.config)

	e.defaultAttrs = buildDefaultAttrs(e.config.DefaultTags)
	return e
}

// NewFromProvider creates an Exporter backed by the given MeterProvider.
// This is intended for unit tests that use a manual reader.
func NewFromProvider(mp *sdkmetric.MeterProvider, opts ...Option) *Exporter {
	e := &Exporter{
		provider:   mp,
		meter:      mp.Meter("test"),
		counters:   make(map[string]metric.Int64Counter),
		gauges:     make(map[string]metric.Float64Gauge),
		histograms: make(map[string]metric.Float64Histogram),
	}
	for _, o := range opts {
		o(e)
	}
	applyDefaults(&e.config)

	e.defaultAttrs = buildDefaultAttrs(e.config.DefaultTags)
	return e
}

// ExportConfigForTest returns the exporter's configuration for test
// assertions. Only available in test builds.
func (e *Exporter) ExportConfigForTest() Config {
	return e.config
}
