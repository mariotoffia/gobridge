package oteltracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oteltracing "github.com/mariotoffia/gobridge/adapters/otel/tracing"
	"github.com/mariotoffia/gobridge/domain/messaging"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	remoteTraceID = "0af7651916cd43dd8448eb211c80319c"
	remoteSpanID  = "b7ad6b7169203331"
	remoteParent  = "00-" + remoteTraceID + "-" + remoteSpanID + "-01"
)

// K1 true-regression: an ingress W3C traceparent must be extracted into
// the OTel context so the started span is a CHILD of the remote parent,
// and the child context must be injectable onto outbound headers with a
// new span ID under the SAME trace ID. Before the fix traceparent was
// only copied to attributes, so the span started a brand-new trace.
func TestPropagation_ExtractStartInject_RoundTrip(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)

	// Use the domain header vocabulary so keys match production.
	headers := map[string]any{
		string(messaging.HeaderTraceParent): remoteParent,
	}

	ctx := tr.Extract(context.Background(), headers)
	ctx, span := tr.StartSpan(ctx, "child")
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	// Child inherits the remote trace ID and is parented on the remote span.
	assert.Equal(t, remoteTraceID, spans[0].SpanContext.TraceID().String(),
		"child span must join the extracted remote trace")
	assert.Equal(t, remoteSpanID, spans[0].Parent.SpanID().String(),
		"child span must be parented on the remote span")
	assert.True(t, spans[0].Parent.IsRemote(), "parent must be flagged remote")

	childSpanID := spans[0].SpanContext.SpanID().String()
	require.NotEqual(t, remoteSpanID, childSpanID, "child must own a fresh span ID")

	// Outbound injection carries the child context downstream.
	out := tr.Inject(ctx, map[string]any{})
	raw, ok := messaging.GetHeaderString(out, messaging.HeaderTraceParent)
	require.True(t, ok, "outbound headers must carry a traceparent")

	tc, ok := messaging.ParseTraceparent(raw)
	require.True(t, ok, "injected traceparent must be valid W3C: %q", raw)
	assert.Equal(t, remoteTraceID, tc.TraceID, "outbound keeps the same trace")
	assert.Equal(t, childSpanID, tc.SpanID, "outbound advertises the child span")
}

// Without an incoming traceparent, StartSpan begins a fresh root trace.
func TestPropagation_NoParent_StartsRoot(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)

	ctx := tr.Extract(context.Background(), map[string]any{})
	_, span := tr.StartSpan(ctx, "root")
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.False(t, spans[0].Parent.IsValid(), "root span must have no parent")
}

// K8: spans exported through a real OTLP HTTP exporter must reach a
// collector; a shutdown flushes the batch. Uses httptest, no sleeps.
func TestTracer_ExportReachesCollector(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr, err := oteltracing.New(context.Background(),
		oteltracing.WithEndpoint(srv.URL),
		oteltracing.WithInsecure(),
	)
	require.NoError(t, err)

	_, span := tr.StartSpan(context.Background(), "exported")
	span.End()

	require.NoError(t, tr.Close(context.Background()))
	assert.GreaterOrEqual(t, hits.Load(), int32(1), "collector must receive at least one export")
}

// K6: sampler ratios outside [0,1] are rejected at construction.
func TestNew_RejectsOutOfRangeSamplerRatio(t *testing.T) {
	t.Parallel()

	_, err := oteltracing.New(context.Background(), oteltracing.WithSamplerRatio(1.5))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")

	_, err = oteltracing.New(context.Background(), oteltracing.WithSamplerRatio(-0.1))
	require.Error(t, err)
}

// MF-9: W3C header lookup must be case-insensitive — a transport that
// stamps "Traceparent" (HTTP-style casing) must not silently break
// trace continuity.
func TestPropagation_ExtractIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)

	headers := map[string]any{
		"Traceparent": remoteParent, // HTTP-style casing, not bridge-lowercase
	}

	ctx := tr.Extract(context.Background(), headers)
	_, span := tr.StartSpan(ctx, "child")
	span.End()

	_ = tp.ForceFlush(context.Background())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, remoteTraceID, spans[0].SpanContext.TraceID().String(),
		"capitalized Traceparent must still join the remote trace")
	assert.Equal(t, remoteSpanID, spans[0].Parent.SpanID().String(),
		"child must be parented on the remote span")
}
