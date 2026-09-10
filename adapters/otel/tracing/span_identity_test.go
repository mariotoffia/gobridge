package oteltracing_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oteltracing "github.com/mariotoffia/gobridge/adapters/otel/tracing"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	w3cTraceIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	w3cSpanIDRE  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// TestSpanIdentity_ExposesActiveSpanIDs proves the adapter span implements
// the OPTIONAL ports.SpanIdentity capability: the runtime stamps
// trace_id/span_id log-correlation fields from the ACTIVE span, so the
// accessors must return the W3C hex identity of the span the SDK actually
// recorded — for a root span (fresh trace) and for a child span joined to an
// upstream traceparent (same trace id, but THIS hop's span id, never the
// upstream parent's).
func TestSpanIdentity_ExposesActiveSpanIDs(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)

	t.Run("root span reports its own fresh identity", func(t *testing.T) {
		exp.Reset()
		_, span := tr.StartSpan(context.Background(), "root")
		ident, ok := span.(ports.SpanIdentity)
		require.True(t, ok, "oteltracing span must implement ports.SpanIdentity")

		gotTrace, gotSpan := ident.TraceID(), ident.SpanID()
		assert.Regexp(t, w3cTraceIDRE, gotTrace, "TraceID must be 32 lowercase hex digits")
		assert.Regexp(t, w3cSpanIDRE, gotSpan, "SpanID must be 16 lowercase hex digits")

		span.End()
		_ = tp.ForceFlush(context.Background())
		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, spans[0].SpanContext.TraceID().String(), gotTrace,
			"TraceID must match the recorded span's trace id")
		assert.Equal(t, spans[0].SpanContext.SpanID().String(), gotSpan,
			"SpanID must match the recorded span's span id")
	})

	t.Run("child span reports upstream trace id but its OWN span id", func(t *testing.T) {
		const upTraceID = "0af7651916cd43dd8448eb211c80319c"
		const upSpanID = "b7ad6b7169203331"
		headers := map[string]any{
			messaging.HeaderTraceParent: "00-" + upTraceID + "-" + upSpanID + "-01",
		}

		ctx := tr.Extract(context.Background(), headers)
		_, span := tr.StartSpan(ctx, "child")
		defer span.End()

		ident, ok := span.(ports.SpanIdentity)
		require.True(t, ok, "oteltracing span must implement ports.SpanIdentity")
		assert.Equal(t, upTraceID, ident.TraceID(), "child joins the upstream trace")
		assert.Regexp(t, w3cSpanIDRE, ident.SpanID())
		assert.NotEqual(t, upSpanID, ident.SpanID(),
			"SpanID must be the ACTIVE span's id, not the upstream parent's")
	})
}

// TestSpanIdentity_UnsampledReturnsEmpty pins the documented contract: an
// unsampled trace exports no spans, so a logged trace_id would dangle — the
// accessors return "" and the runtime falls back to its upstream-traceparent
// stamping (ports.SpanIdentity godoc).
func TestSpanIdentity_UnsampledReturnsEmpty(t *testing.T) {
	t.Parallel()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := oteltracing.NewFromProvider(tp)
	_, span := tr.StartSpan(context.Background(), "unsampled")
	defer span.End()

	ident, ok := span.(ports.SpanIdentity)
	require.True(t, ok, "oteltracing span must implement ports.SpanIdentity")
	assert.Empty(t, ident.TraceID(), "unsampled span must report no trace id")
	assert.Empty(t, ident.SpanID(), "unsampled span must report no span id")
}
