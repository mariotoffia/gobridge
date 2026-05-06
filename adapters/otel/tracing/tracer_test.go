package oteltracing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	oteltracing "github.com/mariotoffia/gobridge/adapters/otel/tracing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Compile-time interface checks.
var (
	_ ports.Tracer = (*oteltracing.Tracer)(nil)
)

// Verifies default tracer configuration uses the expected endpoint, service name, and sampler ratio.
func TestConfig_Defaults(t *testing.T) {
	t.Parallel()

	tr := oteltracing.NewForTest()
	cfg := tr.ExportConfigForTest()

	assert.Equal(t, "http://localhost:4318", cfg.Endpoint)
	assert.Equal(t, "gobridge", cfg.ServiceName)
	assert.Equal(t, 1.0, cfg.SamplerRatio)
}

// Verifies functional options populate the exported test configuration snapshot.
func TestOptions(t *testing.T) {
	t.Parallel()

	opts := []oteltracing.Option{
		oteltracing.WithEndpoint("http://collector:4318"),
		oteltracing.WithServiceName("myservice"),
		oteltracing.WithServiceVersion("1.2.3"),
		oteltracing.WithEnvironment("staging"),
		oteltracing.WithSamplerRatio(0.5),
		oteltracing.WithInsecure(),
		oteltracing.WithHeaders(map[string]string{"X-Token": "abc"}),
	}

	tr := oteltracing.NewForTest(opts...)
	cfg := tr.ExportConfigForTest()

	assert.Equal(t, "http://collector:4318", cfg.Endpoint)
	assert.Equal(t, "myservice", cfg.ServiceName)
	assert.Equal(t, "1.2.3", cfg.ServiceVersion)
	assert.Equal(t, "staging", cfg.Environment)
	assert.Equal(t, 0.5, cfg.SamplerRatio)
	assert.True(t, cfg.Insecure)
	assert.Equal(t, map[string]string{"X-Token": "abc"}, cfg.Headers)
}

// Verifies StartSpan records a span with the given name and domain tags in the SDK exporter.
func TestStartSpan_CreatesSpan(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)
	ctx := context.Background()

	ctx, span := tr.StartSpan(ctx, "test-span",
		shared.Tag{Key: "k1", Value: "v1"},
		shared.Tag{Key: "k2", Value: "v2"},
	)
	span.End()

	_ = tp.ForceFlush(ctx)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "test-span", spans[0].Name)
	assert.Len(t, spans[0].Attributes, 2)
}

// Verifies SetError marks the span as error with status and an exception event.
func TestSpan_SetError(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)
	ctx := context.Background()

	_, span := tr.StartSpan(ctx, "error-span")
	span.SetError(errors.New("boom"))
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, sdktrace.Status{
		Code:        codes.Error,
		Description: "boom",
	}, spans[0].Status)
	require.Len(t, spans[0].Events, 1)
	assert.Equal(t, "exception", spans[0].Events[0].Name)
}

// Verifies AddEvent appends a named event with attributes to the span.
func TestSpan_AddEvent(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)
	ctx := context.Background()

	_, span := tr.StartSpan(ctx, "event-span")
	span.AddEvent("my-event", shared.Tag{Key: "detail", Value: "info"})
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Len(t, spans[0].Events, 1)
	assert.Equal(t, "my-event", spans[0].Events[0].Name)
}

// Verifies Close shuts down the provider without error and that spans
// recorded before Close are visible in the exporter.
func TestTracer_Close_FlushesPendingSpans(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	tr := oteltracing.NewFromProvider(tp)
	ctx := context.Background()

	_, span := tr.StartSpan(ctx, "close-test-span")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "close-test-span", spans[0].Name)

	err := tr.Close(context.Background())
	require.NoError(t, err)
}

// Verifies SetAttributes adds later domain tags to the exported span attributes.
func TestSpan_SetAttributes(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)
	ctx := context.Background()

	_, span := tr.StartSpan(ctx, "attr-span")
	span.SetAttributes(
		shared.Tag{Key: "added", Value: "later"},
	)
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	found := false
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "added" && a.Value.AsString() == "later" {
			found = true
		}
	}
	assert.True(t, found, "expected attribute 'added'='later' on span")
}
