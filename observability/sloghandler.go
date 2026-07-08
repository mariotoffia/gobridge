package observability

import (
	"context"
	"fmt"
	"log/slog"
)

var _ slog.Handler = (*CorrelationHandler)(nil)

// CorrelationHandler is an slog.Handler that injects correlation_id,
// trace_id, and span_id from context into every log record.
//
// The correlation fields are always composed FLAT at the record root,
// even when the handler has open groups (WithGroup). A grouped record
// would otherwise nest correlation_id under the group (e.g.
// $.request.correlation_id), which the documented CloudWatch Insights
// queries on $.correlation_id would miss.
type CorrelationHandler struct {
	// inner is the base handler with every WithAttrs/WithGroup already
	// applied. It is the fast path used whenever no group is open, so the
	// common (group-less) case costs nothing extra.
	inner slog.Handler
	// root is the base handler with NO groups or attrs applied.
	// Correlation attrs are attached here, at the root, then the recorded
	// ops are replayed so a grouped record keeps its own attrs grouped
	// while correlation stays flat.
	root slog.Handler
	// ops records WithAttrs/WithGroup calls in order for the replay path.
	ops []corrOp
	// grouped is true once WithGroup has been called. Only then is the
	// replay path taken; WithAttrs alone keeps the record at the root, so
	// the fast path still applies.
	grouped bool
}

// corrOp is one recorded WithAttrs or WithGroup call. Exactly one of the
// fields is set: a non-empty group means WithGroup(group); non-nil attrs
// mean WithAttrs(attrs).
type corrOp struct {
	group string
	attrs []slog.Attr
}

// NewCorrelationHandler wraps inner, adding correlation fields from
// context to each handled record.
//
// PRECONDITION: inner must have NO open groups — pass a root handler and
// apply any groups via the returned handler's WithGroup. The flat-at-root
// guarantee is tracked from this wrapper's own WithGroup calls; a group
// already baked into inner is invisible here, so the fast path would nest
// correlation_id under it (e.g. $.request.correlation_id) and silently
// defeat the $.correlation_id CloudWatch Insights queries.
func NewCorrelationHandler(inner slog.Handler) *CorrelationHandler {
	if inner == nil {
		panic("observability: inner handler must not be nil")
	}
	return &CorrelationHandler{inner: inner, root: inner}
}

func (h *CorrelationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *CorrelationHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := correlationAttrs(ctx)

	if !h.grouped || len(attrs) == 0 {
		// Fast path: no open group (or nothing to inject). Correlation
		// attrs land at the record root exactly as a flat handler would.
		if len(attrs) > 0 {
			record.AddAttrs(attrs...)
		}
		if err := h.inner.Handle(ctx, record); err != nil {
			return fmt.Errorf("correlation handler: %w", err)
		}
		return nil
	}

	// Replay path: attach correlation attrs on the group-less root so
	// they stay flat, then replay the recorded WithAttrs/WithGroup ops so
	// the record's own attrs remain grouped as configured.
	handler := h.root.WithAttrs(attrs)
	for _, op := range h.ops {
		if op.group != "" {
			handler = handler.WithGroup(op.group)
		} else {
			handler = handler.WithAttrs(op.attrs)
		}
	}
	if err := handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("correlation handler: %w", err)
	}
	return nil
}

func (h *CorrelationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &CorrelationHandler{
		inner:   h.inner.WithAttrs(attrs),
		root:    h.root,
		ops:     appendCorrOp(h.ops, corrOp{attrs: attrs}),
		grouped: h.grouped,
	}
}

func (h *CorrelationHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		// slog contract: an empty group name is a no-op.
		return h
	}
	return &CorrelationHandler{
		inner:   h.inner.WithGroup(name),
		root:    h.root,
		ops:     appendCorrOp(h.ops, corrOp{group: name}),
		grouped: true,
	}
}

// correlationAttrs collects the correlation fields present on ctx, in a
// stable order, as flat (group-less) attributes.
func correlationAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if id := CorrelationIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("correlation_id", id))
	}
	if id := TraceIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("trace_id", id))
	}
	if id := SpanIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("span_id", id))
	}
	return attrs
}

// appendCorrOp returns a fresh slice with op appended, never aliasing the
// parent's backing array so sibling handlers derived from a common parent
// cannot bleed ops into one another.
func appendCorrOp(ops []corrOp, op corrOp) []corrOp {
	out := make([]corrOp, len(ops)+1)
	copy(out, ops)
	out[len(ops)] = op
	return out
}
