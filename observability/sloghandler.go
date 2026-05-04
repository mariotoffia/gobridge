package observability

import (
	"context"
	"fmt"
	"log/slog"
)

var _ slog.Handler = (*CorrelationHandler)(nil)

// CorrelationHandler is an slog.Handler that injects correlation_id,
// trace_id, and span_id from context into every log record.
type CorrelationHandler struct {
	inner slog.Handler
}

// NewCorrelationHandler wraps inner, adding correlation fields from
// context to each handled record.
func NewCorrelationHandler(inner slog.Handler) *CorrelationHandler {
	if inner == nil {
		panic("observability: inner handler must not be nil")
	}
	return &CorrelationHandler{inner: inner}
}

func (h *CorrelationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *CorrelationHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := CorrelationIDFromContext(ctx); id != "" {
		record.AddAttrs(slog.String("correlation_id", id))
	}
	if id := TraceIDFromContext(ctx); id != "" {
		record.AddAttrs(slog.String("trace_id", id))
	}
	if id := SpanIDFromContext(ctx); id != "" {
		record.AddAttrs(slog.String("span_id", id))
	}
	if err := h.inner.Handle(ctx, record); err != nil {
		return fmt.Errorf("correlation handler: %w", err)
	}
	return nil
}

func (h *CorrelationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CorrelationHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *CorrelationHandler) WithGroup(name string) slog.Handler {
	return &CorrelationHandler{inner: h.inner.WithGroup(name)}
}
