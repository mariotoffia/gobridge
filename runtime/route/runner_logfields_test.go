package route

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/observability"
	"github.com/mariotoffia/gobridge/ports"
)

// This file pins log-correlation fix: doHandleDelivery must stamp the
// observability trace_id/span_id fields from the ACTIVE span when the tracer
// exposes ports.SpanIdentity, falling back to the upstream traceparent only
// when it does not (NoopTracer). Before the fix the fields always came from
// the upstream traceparent — root deliveries logged no trace_id at all, and
// child deliveries logged the PARENT's span id instead of this hop's.

// identitySpan is a ports.Span exposing a fixed ports.SpanIdentity.
type identitySpan struct {
	noopTestSpan
	traceID, spanID string
}

func (s identitySpan) TraceID() string { return s.traceID }
func (s identitySpan) SpanID() string  { return s.spanID }

// identityTracer starts identitySpan spans; Extract/Inject pass through.
type identityTracer struct{ traceID, spanID string }

func (t identityTracer) StartSpan(ctx context.Context, _ string, _ ...shared.Tag) (context.Context, ports.Span) {
	return ctx, identitySpan{traceID: t.traceID, spanID: t.spanID}
}
func (t identityTracer) Extract(ctx context.Context, _ map[string]any) context.Context { return ctx }
func (t identityTracer) Inject(_ context.Context, h map[string]any) map[string]any     { return h }
func (t identityTracer) Close(context.Context) error                                   { return nil }

// ctxCaptureHook records the context passed to OnAttempt — the first hook
// point after the runner stamps its log-correlation fields, i.e. the same
// context every downstream log line and outbound call observes.
type ctxCaptureHook struct {
	ports.NoopDeliveryHook
	attemptCtx context.Context //nolint:containedctx // test capture of the stamped ctx
}

func (h *ctxCaptureHook) OnAttempt(ctx context.Context, _ ports.DeliveryAttempt) {
	h.attemptCtx = ctx
}

func TestHandleDelivery_LogFieldsFromActiveSpan(t *testing.T) {
	const activeTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const activeSpanID = "bbbbbbbbbbbbbbbb"
	const upstreamTraceID = "0af7651916cd43dd8448eb211c80319c"
	const upstreamSpanID = "b7ad6b7169203331"
	const upstreamTP = "00-" + upstreamTraceID + "-" + upstreamSpanID + "-01"

	newRunner := func(tr ports.Tracer, hook ports.DeliveryHook) *RouteRunner {
		return NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID:  "r1",
			Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
			Sender:   stubSender{},
			Bindings: []routing.DestinationBinding{{ID: "b1", Address: "topic"}},
			Tracer:   tr,
			Hook:     hook,
		})
	}
	deliver := func(t *testing.T, r *RouteRunner, headers map[string]any) {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("p"), Headers: headers})
		if err := r.HandleDelivery(context.Background(), &stubDelivery{env: env}); err != nil {
			t.Fatalf("HandleDelivery returned error: %v", err)
		}
	}

	t.Run("root delivery gets the active span's identity", func(t *testing.T) {
		hook := &ctxCaptureHook{}
		r := newRunner(identityTracer{traceID: activeTraceID, spanID: activeSpanID}, hook)

		deliver(t, r, nil) // no upstream traceparent

		if got := observability.TraceIDFromContext(hook.attemptCtx); got != activeTraceID {
			t.Errorf("root delivery trace_id = %q, want active span's %q", got, activeTraceID)
		}
		if got := observability.SpanIDFromContext(hook.attemptCtx); got != activeSpanID {
			t.Errorf("root delivery span_id = %q, want active span's %q", got, activeSpanID)
		}
	})

	t.Run("active span identity wins over upstream traceparent", func(t *testing.T) {
		hook := &ctxCaptureHook{}
		r := newRunner(identityTracer{traceID: upstreamTraceID, spanID: activeSpanID}, hook)

		deliver(t, r, map[string]any{messaging.HeaderTraceParent: upstreamTP})

		if got := observability.TraceIDFromContext(hook.attemptCtx); got != upstreamTraceID {
			t.Errorf("trace_id = %q, want %q (same trace, joined via Extract)", got, upstreamTraceID)
		}
		if got := observability.SpanIDFromContext(hook.attemptCtx); got != activeSpanID {
			t.Errorf("span_id = %q, want THIS hop's %q, not upstream parent's %q", got, activeSpanID, upstreamSpanID)
		}
	})

	t.Run("tracer without SpanIdentity falls back to upstream traceparent", func(t *testing.T) {
		hook := &ctxCaptureHook{}
		r := newRunner(&fakeTracer{}, hook) // returns noopTestSpan: no identity

		deliver(t, r, map[string]any{messaging.HeaderTraceParent: upstreamTP})

		if got := observability.TraceIDFromContext(hook.attemptCtx); got != upstreamTraceID {
			t.Errorf("fallback trace_id = %q, want upstream %q", got, upstreamTraceID)
		}
		if got := observability.SpanIDFromContext(hook.attemptCtx); got != upstreamSpanID {
			t.Errorf("fallback span_id = %q, want upstream %q", got, upstreamSpanID)
		}
	})

	t.Run("identity returning empty falls back to upstream traceparent", func(t *testing.T) {
		hook := &ctxCaptureHook{}
		r := newRunner(identityTracer{}, hook) // capability present, ids empty (unsampled)

		deliver(t, r, map[string]any{messaging.HeaderTraceParent: upstreamTP})

		if got := observability.TraceIDFromContext(hook.attemptCtx); got != upstreamTraceID {
			t.Errorf("unsampled trace_id = %q, want upstream %q", got, upstreamTraceID)
		}
		if got := observability.SpanIDFromContext(hook.attemptCtx); got != upstreamSpanID {
			t.Errorf("unsampled span_id = %q, want upstream %q", got, upstreamSpanID)
		}
	})
}
