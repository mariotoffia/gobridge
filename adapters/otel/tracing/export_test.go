package oteltracing

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewForTest creates a Tracer with options applied and defaults set,
// without establishing a network connection to a collector.
func NewForTest(opts ...Option) *Tracer {
	t := &Tracer{}
	for _, o := range opts {
		o(t)
	}
	applyDefaults(&t.config)
	return t
}

// NewFromProvider creates a Tracer backed by the given TracerProvider.
// This is intended for unit tests that use an in-memory span exporter.
func NewFromProvider(tp *sdktrace.TracerProvider) *Tracer {
	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("test"),
	}
}

// ExportConfigForTest returns the tracer's configuration for test
// assertions. Only available in test builds.
func (t *Tracer) ExportConfigForTest() Config {
	return t.config
}
