package otelmetrics

import (
	"slices"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// NewForTest creates an Exporter with options applied and defaults set,
// without establishing a network connection to a collector. The
// returned Exporter has no meterClient — callers that exercise the
// emit methods should use NewFromProvider instead.
func NewForTest(opts ...Option) *Exporter {
	e := &Exporter{}
	for _, o := range opts {
		o(e)
	}
	applyDefaults(&e.config)
	return e
}

// NewFromProvider creates an Exporter backed by the given MeterProvider.
// This is intended for unit tests that use a manual reader.
func NewFromProvider(mp *sdkmetric.MeterProvider, opts ...Option) *Exporter {
	e := &Exporter{}
	for _, o := range opts {
		o(e)
	}
	applyDefaults(&e.config)
	e.client = newMeterClientFromProvider(mp, e.config)
	return e
}

// ExportConfigForTest returns the exporter's configuration for test
// assertions. Only available in test builds.
func (e *Exporter) ExportConfigForTest() Config {
	return e.config
}

// LatencyBucketBoundariesMsForTest returns a copy of the Timer
// histogram bucket boundaries so tests can assert the emitted bounds
// without depending on the internal literal.
func LatencyBucketBoundariesMsForTest() []float64 {
	return slices.Clone(latencyBucketBoundariesMs)
}
