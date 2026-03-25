package observability_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/observability"
	"github.com/stretchr/testify/assert"
)

// Verifies CorrelationIDFromContext returns the value set by WithCorrelationID.
func TestCorrelationIDRoundTrip(t *testing.T) {
	ctx := observability.WithCorrelationID(context.Background(), "corr-123")
	assert.Equal(t, "corr-123", observability.CorrelationIDFromContext(ctx))
}

// Verifies TraceIDFromContext returns the value set by WithTraceID.
func TestTraceIDRoundTrip(t *testing.T) {
	ctx := observability.WithTraceID(context.Background(), "trace-abc")
	assert.Equal(t, "trace-abc", observability.TraceIDFromContext(ctx))
}

// Verifies SpanIDFromContext returns the value set by WithSpanID.
func TestSpanIDRoundTrip(t *testing.T) {
	ctx := observability.WithSpanID(context.Background(), "span-xyz")
	assert.Equal(t, "span-xyz", observability.SpanIDFromContext(ctx))
}

// Verifies ID accessors return empty strings on a bare context.Background.
func TestMissingValuesReturnEmpty(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, observability.CorrelationIDFromContext(ctx))
	assert.Empty(t, observability.TraceIDFromContext(ctx))
	assert.Empty(t, observability.SpanIDFromContext(ctx))
}

// Verifies correlation, trace, and span IDs can be layered on the same context and read independently.
func TestContextLayering(t *testing.T) {
	ctx := context.Background()
	ctx = observability.WithCorrelationID(ctx, "corr-1")
	ctx = observability.WithTraceID(ctx, "trace-1")
	ctx = observability.WithSpanID(ctx, "span-1")

	assert.Equal(t, "corr-1", observability.CorrelationIDFromContext(ctx))
	assert.Equal(t, "trace-1", observability.TraceIDFromContext(ctx))
	assert.Equal(t, "span-1", observability.SpanIDFromContext(ctx))
}

// Verifies a second WithCorrelationID replaces the previous correlation ID on the context.
func TestOverwrite(t *testing.T) {
	ctx := observability.WithCorrelationID(context.Background(), "first")
	ctx = observability.WithCorrelationID(ctx, "second")
	assert.Equal(t, "second", observability.CorrelationIDFromContext(ctx))
}
