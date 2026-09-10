package oteltracing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// true-regression: a sampler ratio of 0.0 must disable sampling of
// new root traces. Before the fix, WithSamplerRatio(0) was reset to 1.0
// by applyDefaults, so every root span was sampled.
func TestSamplerFromConfig_ZeroDisablesRootSampling(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	zero := 0.0
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(samplerFromConfig(Config{SamplerRatio: &zero})),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("t").Start(context.Background(), "root")
	span.End()
	_ = tp.ForceFlush(context.Background())

	assert.Empty(t, exp.GetSpans(), "ratio 0 must record no root spans")
}

// Counterpart: ratio 1.0 records the root span.
func TestSamplerFromConfig_OneSamplesRoot(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	one := 1.0
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(samplerFromConfig(Config{SamplerRatio: &one})),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("t").Start(context.Background(), "root")
	span.End()
	_ = tp.ForceFlush(context.Background())

	assert.Len(t, exp.GetSpans(), 1)
}

// applyDefaults must preserve an explicit 0.0 rather than treating it as
// unset (the root cause of the bug).
func TestApplyDefaults_SamplerRatioZeroPreserved(t *testing.T) {
	t.Parallel()

	zero := 0.0
	cfg := Config{SamplerRatio: &zero}
	applyDefaults(&cfg)

	require.NotNil(t, cfg.SamplerRatio)
	assert.Equal(t, 0.0, *cfg.SamplerRatio)
}

// applyDefaults fills a nil ratio with 1.0.
func TestApplyDefaults_SamplerRatioNilDefaultsToOne(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	applyDefaults(&cfg)

	require.NotNil(t, cfg.SamplerRatio)
	assert.Equal(t, 1.0, *cfg.SamplerRatio)
}

// WithSamplerRatio(0) records an explicit zero on the config (not unset).
func TestWithSamplerRatio_ZeroIsExplicit(t *testing.T) {
	t.Parallel()

	tr := NewForTest(WithSamplerRatio(0))
	cfg := tr.ExportConfigForTest()

	require.NotNil(t, cfg.SamplerRatio)
	assert.Equal(t, 0.0, *cfg.SamplerRatio)
}

// an OTLP endpoint env var suppresses the hardcoded default so the
// SDK can honor it.
func TestApplyDefaults_HonorsEnvEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.internal:4318")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := Config{}
	applyDefaults(&cfg)

	assert.Empty(t, cfg.Endpoint, "env endpoint must not be overridden by a hardcoded default")
}

// OTEL_SERVICE_NAME suppresses the hardcoded service name default.
func TestApplyDefaults_HonorsEnvServiceName(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "svc-from-env")

	cfg := Config{}
	applyDefaults(&cfg)

	assert.Empty(t, cfg.ServiceName, "env service name must not be overridden by a hardcoded default")
}

// true-regression: span export failures are surfaced via the error
// handler instead of being silently swallowed.
func TestObservedSpanExporter_ReportsExportError(t *testing.T) {
	t.Parallel()

	var got error
	obs := &observedSpanExporter{
		SpanExporter: failingSpanExporter{},
		onError:      func(err error) { got = err },
	}

	err := obs.ExportSpans(context.Background(), nil)
	require.Error(t, err)
	require.Error(t, got, "error handler must observe the export failure")
	assert.Contains(t, got.Error(), "otel-tracing: export spans")
}

// A nil error handler must not panic.
func TestObservedSpanExporter_NilHandlerNoPanic(t *testing.T) {
	t.Parallel()

	obs := &observedSpanExporter{SpanExporter: failingSpanExporter{}}
	assert.Error(t, obs.ExportSpans(context.Background(), nil))
}

type failingSpanExporter struct{}

func (failingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("boom")
}

func (failingSpanExporter) Shutdown(context.Context) error { return nil }

// carrier lookup is case-insensitive per W3C; writes stay
// lowercase (bridge header vocabulary).
func TestHeaderCarrier_GetCaseInsensitive(t *testing.T) {
	t.Parallel()

	c := headerCarrier{"Traceparent": "00-abc-def-01", "tracestate": "a=b"}
	assert.Equal(t, "00-abc-def-01", c.Get("traceparent"), "case-insensitive fallback")
	assert.Equal(t, "a=b", c.Get("tracestate"), "exact match fast path")
	assert.Empty(t, c.Get("missing"))

	// Non-string values are ignored, not panicked on.
	c["baggage"] = 42
	assert.Empty(t, c.Get("baggage"))

	// Writes keep the lowercase bridge vocabulary.
	c.Set("traceparent", "00-new-new-01")
	assert.Equal(t, "00-new-new-01", c["traceparent"])
}

// span-export failures must not be silent by default — an unset
// error handler falls back to slog warn logging, and an explicit
// WithErrorHandler(nil) opts out.
func TestApplyDefaults_ErrorHandlerDefaultsToWarnLogger(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	applyDefaults(&cfg)
	require.NotNil(t, cfg.errorHandler, "unset handler must default to slog warn logging")
	// The default handler must be safe to invoke.
	cfg.errorHandler(errors.New("synthetic export failure"))

	tr := NewForTest(WithErrorHandler(nil))
	assert.Nil(t, tr.config.errorHandler, "explicit nil handler must suppress reporting")
	assert.True(t, tr.config.errorHandlerSet)
}
