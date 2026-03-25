package observability_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/observability"
	"github.com/stretchr/testify/assert"
)

func TestCorrelationIDRoundTrip(t *testing.T) {
	ctx := observability.WithCorrelationID(context.Background(), "corr-123")
	assert.Equal(t, "corr-123", observability.CorrelationIDFromContext(ctx))
}

func TestTraceIDRoundTrip(t *testing.T) {
	ctx := observability.WithTraceID(context.Background(), "trace-abc")
	assert.Equal(t, "trace-abc", observability.TraceIDFromContext(ctx))
}

func TestSpanIDRoundTrip(t *testing.T) {
	ctx := observability.WithSpanID(context.Background(), "span-xyz")
	assert.Equal(t, "span-xyz", observability.SpanIDFromContext(ctx))
}

func TestMissingValuesReturnEmpty(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, observability.CorrelationIDFromContext(ctx))
	assert.Empty(t, observability.TraceIDFromContext(ctx))
	assert.Empty(t, observability.SpanIDFromContext(ctx))
}

func TestContextLayering(t *testing.T) {
	ctx := context.Background()
	ctx = observability.WithCorrelationID(ctx, "corr-1")
	ctx = observability.WithTraceID(ctx, "trace-1")
	ctx = observability.WithSpanID(ctx, "span-1")

	assert.Equal(t, "corr-1", observability.CorrelationIDFromContext(ctx))
	assert.Equal(t, "trace-1", observability.TraceIDFromContext(ctx))
	assert.Equal(t, "span-1", observability.SpanIDFromContext(ctx))
}

func TestOverwrite(t *testing.T) {
	ctx := observability.WithCorrelationID(context.Background(), "first")
	ctx = observability.WithCorrelationID(ctx, "second")
	assert.Equal(t, "second", observability.CorrelationIDFromContext(ctx))
}
