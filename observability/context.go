package observability

import "context"

type contextKey int

const (
	correlationIDKey contextKey = iota
	traceIDKey
	spanIDKey
)

// CorrelationIDFromContext returns the correlation ID stored in ctx, or "" if none.
func CorrelationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationIDKey).(string)
	return v
}

// WithCorrelationID returns a child context carrying the given correlation ID.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// TraceIDFromContext returns the distributed trace ID stored in ctx, or "" if none.
func TraceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey).(string)
	return v
}

// WithTraceID returns a child context carrying the given trace ID.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// SpanIDFromContext returns the span ID stored in ctx, or "" if none.
func SpanIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(spanIDKey).(string)
	return v
}

// WithSpanID returns a child context carrying the given span ID.
func WithSpanID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, spanIDKey, id)
}
