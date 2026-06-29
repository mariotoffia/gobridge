package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/observability"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

func makeRunner(t *testing.T, opts ...func(*route.RouteRunnerConfig)) (*FakeReceiver, *FakeSender, *FakeDLQStore, *FakeOutboxStore, *route.RouteRunner) {
	t.Helper()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	outbox := NewFakeOutboxStore()

	cfg := route.RouteRunnerConfig{
		RouteID:     "test-route",
		Policy:      routing.RoutePolicy{}.WithDefaults(),
		Receiver:    receiver,
		Sender:      sender,
		OutboxStore: outbox,
		DLQ:         dlq.New(dlqStore),
		InstanceID:  "bridge-1",
	}
	for _, o := range opts {
		o(&cfg)
	}
	runner := route.NewRouteRunnerFromConfig(cfg)
	return receiver, sender, dlqStore, outbox, runner
}

// TestRouteRunner_DirectHold_HappyPath verifies direct hold delivers a message, sends once, and acks the delivery.
func TestRouteRunner_DirectHold_HappyPath(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1", Payload: []byte("hello")})
	del := NewFakeDelivery(env)

	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	waitFor(t, time.Second, "delivery acked and sent", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", sender.SentCount())
	}
	if !del.IsAcked() {
		t.Fatal("delivery should be acked")
	}
}

// TestRouteRunner_DirectHold_TransientSendError verifies transient send failure retries the delivery without acking.
func TestRouteRunner_DirectHold_TransientSendError(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
	})
	sender.SendErr = shared.ErrUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-2"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery retried on transient send error", del.IsRetried)

	if !del.IsRetried() {
		t.Fatal("expected delivery to be retried on transient error")
	}
	if del.IsAcked() {
		t.Fatal("should not ack on transient error")
	}
}

// TestRouteRunner_DirectHold_PermanentSendError verifies permanent send failure moves the message to DLQ and acks.
func TestRouteRunner_DirectHold_PermanentSendError(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t)
	sender.SendErr = shared.ErrNotAuthorized

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-3"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "DLQ write and delivery ack", func() bool {
		return dlqStore.Count() == 1 && del.IsAcked()
	})

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
	if !del.IsAcked() {
		t.Fatal("should ack after DLQ on permanent error")
	}
}

// TestRouteRunner_ExpiredMessage verifies expired envelopes skip send, land on DLQ, and ack to stop redelivery.
func TestRouteRunner_ExpiredMessage(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-expired"})
		_ = e.SetExpiry(time.Now().Add(-time.Second))
		return e
	}()
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "expired message DLQ and ack", func() bool {
		return del.IsAcked() && dlqStore.Count() == 1 && sender.SentCount() == 0
	})

	if sender.SentCount() != 0 {
		t.Fatal("expired message should not be sent")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry for expired message, got %d", dlqStore.Count())
	}
	if !del.IsAcked() {
		t.Fatal("expired message should be acked to prevent redelivery")
	}
}

// TestRouteRunner_HeaderInjection verifies reserved correlation headers are regenerated while custom headers are kept.
func TestRouteRunner_HeaderInjection(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID: "msg-headers",
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "injected-by-attacker",
			"custom-header":               "keep-me",
		},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery acked and message sent", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatal("expected 1 sent message")
	}
	sent := sender.GetSent()[0]
	if _, ok := sent.Headers()["custom-header"]; !ok {
		t.Fatal("custom header should be preserved")
	}
	corrID, ok := sent.Headers()[messaging.HeaderCorrelationID].(string)
	if !ok || corrID == "injected-by-attacker" {
		t.Fatal("reserved header from external source should be stripped and regenerated")
	}
}

// TestRouteRunner_ProcessorError_Permanent verifies permanent processor errors route to DLQ and ack the delivery.
func TestRouteRunner_ProcessorError_Permanent(t *testing.T) {
	receiver, _, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "reject", ProcessErr: shared.ErrInvalidPayload},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-bad"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "DLQ and ack after permanent processor error", func() bool {
		return dlqStore.Count() == 1 && del.IsAcked()
	})

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
	if !del.IsAcked() {
		t.Fatal("permanent processor error should ack")
	}
}

// TestRouteRunner_ProcessorError_Transient verifies transient processor errors trigger source retry without ack.
func TestRouteRunner_ProcessorError_Transient(t *testing.T) {
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "flaky", ProcessErr: shared.ErrUnavailable},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "transient processor error retry", del.IsRetried)

	if !del.IsRetried() {
		t.Fatal("transient processor error should retry via source")
	}
}

// TestRouteRunner_ProcessorError_MessageFiltered_Drop verifies filtered messages ack silently
// without send, retry, or DLQ when OnPermanentFailure is set to Drop.
func TestRouteRunner_ProcessorError_MessageFiltered_Drop(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.OnPermanentFailure = routing.FailureDrop
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "filter", ProcessErr: shared.ErrMessageFiltered},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-filtered"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "filtered message acked", del.IsAcked)

	if !del.IsAcked() {
		t.Fatal("filtered message should be acked (silent drop)")
	}
	if del.IsRetried() {
		t.Fatal("filtered message should not be retried")
	}
	if dlqStore.Count() != 0 {
		t.Fatalf("filtered message should not go to DLQ, got %d entries", dlqStore.Count())
	}
	if sender.SentCount() != 0 {
		t.Fatalf("filtered message should not be sent, got %d", sender.SentCount())
	}
}

// TestRouteRunner_ProcessorError_MessageFiltered_DLQ verifies filtered messages are
// written to DLQ and acked when OnPermanentFailure is set to DLQ.
func TestRouteRunner_ProcessorError_MessageFiltered_DLQ(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.OnPermanentFailure = routing.FailureDLQ
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "filter", ProcessErr: shared.ErrMessageFiltered},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-filtered-dlq"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "filtered message DLQ and ack", func() bool {
		return dlqStore.Count() == 1 && del.IsAcked()
	})

	if !del.IsAcked() {
		t.Fatal("filtered message with DLQ policy should be acked")
	}
	if del.IsRetried() {
		t.Fatal("filtered message should not be retried")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("filtered message with DLQ policy should have 1 DLQ entry, got %d", dlqStore.Count())
	}
	if sender.SentCount() != 0 {
		t.Fatalf("filtered message should not be sent, got %d", sender.SentCount())
	}
}

// TestRouteRunner_Tracer_SpanLifecycle verifies span name, completion, success state, and route/envelope attributes.
func TestRouteRunner_Tracer_SpanLifecycle(t *testing.T) {
	tracer := &FakeTracer{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-traced", Payload: []byte("hello")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery acked and span ended", func() bool {
		s := tracer.LastSpan()
		return del.IsAcked() && s != nil && s.IsEnded()
	})

	if tracer.SpanCount() != 1 {
		t.Fatalf("expected 1 span, got %d", tracer.SpanCount())
	}

	span := tracer.LastSpan()
	if span == nil {
		t.Fatal("expected a span")
	}
	name, ended, attrs, spanErr := span.Inspect()
	if name != "bridge.handleDelivery" {
		t.Fatalf("expected span name bridge.handleDelivery, got %q", name)
	}
	if !ended {
		t.Fatal("span should be ended after delivery completes")
	}
	if spanErr != nil {
		t.Fatalf("span should not have error on happy path, got %v", spanErr)
	}

	hasRouteTag := false
	hasEnvelopeTag := false
	for _, a := range attrs {
		if a.Key == shared.TagKeyRouteID && a.Value == "test-route" {
			hasRouteTag = true
		}
		if a.Key == "envelope_id" && a.Value == "msg-traced" {
			hasEnvelopeTag = true
		}
	}
	if !hasRouteTag {
		t.Fatal("span should have route_id attribute")
	}
	if !hasEnvelopeTag {
		t.Fatal("span should have envelope_id attribute")
	}
}

// TestRouteRunner_Tracer_TraceContextExtraction verifies W3C traceparent is parsed into trace_id span attributes.
func TestRouteRunner_Tracer_TraceContextExtraction(t *testing.T) {
	tracer := &FakeTracer{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID: "msg-w3c",
		Headers: map[string]any{
			"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery acked and span ended for trace extraction", func() bool {
		s := tracer.LastSpan()
		return del.IsAcked() && s != nil && s.IsEnded()
	})

	span := tracer.LastSpan()
	if span == nil {
		t.Fatal("expected a span")
	}

	_, _, attrs, _ := span.Inspect()
	hasTraceID := false
	for _, a := range attrs {
		if a.Key == "trace_id" && a.Value == "0af7651916cd43dd8448eb211c80319c" {
			hasTraceID = true
		}
	}
	if !hasTraceID {
		t.Fatal("span should have trace_id attribute from traceparent header")
	}
}

// TestRouteRunner_Tracer_ContextEnrichment verifies correlation, trace, and span IDs reach processors from traceparent.
func TestRouteRunner_Tracer_ContextEnrichment(t *testing.T) {
	tracer := &FakeTracer{}
	var capturedCorrID, capturedTraceID, capturedSpanID string

	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
		cfg.Processors = []ports.Processor{
			&FakeProcessor{
				NameVal: "capture-ctx",
				ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
					capturedCorrID = observability.CorrelationIDFromContext(ctx)
					capturedTraceID = observability.TraceIDFromContext(ctx)
					capturedSpanID = observability.SpanIDFromContext(ctx)
					return next(ctx, env)
				},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID: "msg-ctx",
		Headers: map[string]any{
			"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery acked for context enrichment", del.IsAcked)

	if capturedCorrID == "" {
		t.Fatal("correlation ID should be set in context")
	}
	if capturedTraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("expected trace ID from traceparent, got %q", capturedTraceID)
	}
	if capturedSpanID != "b7ad6b7169203331" {
		t.Fatalf("expected span ID from traceparent, got %q", capturedSpanID)
	}
}

// TestRouteRunner_Tracer_ErrorRecording verifies delivery ack failures are recorded on the span.
func TestRouteRunner_Tracer_ErrorRecording(t *testing.T) {
	tracer := &FakeTracer{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-err"}))
	del.AckErr = fmt.Errorf("ack transport failure")
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery ack attempted and span ended", func() bool {
		s := tracer.LastSpan()
		return del.IsAcked() && s != nil && s.IsEnded()
	})

	span := tracer.LastSpan()
	if span == nil {
		t.Fatal("expected a span")
	}
	_, ended, _, spanErr := span.Inspect()
	if !ended {
		t.Fatal("span should be ended")
	}
	if spanErr == nil {
		t.Fatal("span should record error on delivery failure")
	}
}

// TestRouteRunner_Tracer_ProcessorErrorRecording verifies permanent processor errors are recorded on the span.
func TestRouteRunner_Tracer_ProcessorErrorRecording(t *testing.T) {
	tracer := &FakeTracer{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "fail", ProcessErr: shared.ErrInvalidPayload},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-proc-err"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "delivery acked and span ended after processor error", func() bool {
		s := tracer.LastSpan()
		return del.IsAcked() && s != nil && s.IsEnded()
	})

	span := tracer.LastSpan()
	if span == nil {
		t.Fatal("expected a span")
	}
	_, _, _, spanErr := span.Inspect()
	if spanErr == nil {
		t.Fatal("span should record processor error")
	}
}

// TestRouteRunner_Tracer_FilteredNoError verifies filtered deliveries end the span without an error.
func TestRouteRunner_Tracer_FilteredNoError(t *testing.T) {
	tracer := &FakeTracer{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Tracer = tracer
		cfg.Processors = []ports.Processor{
			&FakeProcessor{NameVal: "filter", ProcessErr: shared.ErrMessageFiltered},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-filtered-trace"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "filtered delivery acked and span ended", func() bool {
		s := tracer.LastSpan()
		return del.IsAcked() && s != nil && s.IsEnded()
	})

	span := tracer.LastSpan()
	if span == nil {
		t.Fatal("expected a span")
	}
	_, ended, _, spanErr := span.Inspect()
	if !ended {
		t.Fatal("span should be ended")
	}
	if spanErr != nil {
		t.Fatalf("filtered messages should NOT record error on span, got %v", spanErr)
	}
}

// TestRouteRunner_SharedOutbox_HappyPath verifies shared outbox persists one record and acks the source delivery.
func TestRouteRunner_SharedOutbox_HappyPath(t *testing.T) {
	receiver, _, _, outbox, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "bind-1", Address: "topic/a"},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-outbox"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "outbox persist and source ack", func() bool {
		return outbox.RecordCount() == 1 && del.IsAcked()
	})

	if outbox.RecordCount() != 1 {
		t.Fatalf("expected 1 outbox record, got %d", outbox.RecordCount())
	}
	if !del.IsAcked() {
		t.Fatal("source should be acked after outbox persist")
	}
}

// TestRouteRunner_SharedOutbox_DuplicatePersist verifies a second delivery for the same envelope id is still acked after duplicate persist.
func TestRouteRunner_SharedOutbox_DuplicatePersist(t *testing.T) {
	outbox := NewFakeOutboxStore()
	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.OutboxStore = outbox
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "bind-dup", Address: "topic/dup"},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-dup"})
	del1 := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del1)
	waitFor(t, time.Second, "first delivery acked", del1.IsAcked)

	del2 := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del2)
	waitFor(t, time.Second, "second delivery acked after duplicate persist", del2.IsAcked)

	if !del2.IsAcked() {
		t.Fatal("duplicate persist should still ack the delivery")
	}
}

// TestRouteRunner_DirectHold_WithResolver verifies header-driven resolver output becomes the outbound subject.
func TestRouteRunner_DirectHold_WithResolver(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Bindings = []routing.DestinationBinding{
			{ID: "bind-a", Transport: "mqtt", SessionID: "sess-a", Address: "factory/a/orders/{device_id}"},
			{ID: "bind-b", Transport: "mqtt", SessionID: "sess-b", Address: "factory/b/orders/{device_id}"},
		}
		cfg.Resolver = runtime.NewBindingResolver(cfg.Bindings, runtime.MatchByHeader("factory", map[string]string{
			"A": "bind-a",
			"B": "bind-b",
		}))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-resolve",
		Headers: map[string]any{"factory": "A", "device_id": "42"},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "send and ack with resolver", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", sender.SentCount())
	}
	out := sender.GetOutbound()
	if out[0].Address != "factory/a/orders/42" {
		t.Fatalf("expected resolved Address, got %q", out[0].Address)
	}
	if out[0].Envelope.Subject() != "" {
		t.Fatalf("source envelope had no Subject; outbound must preserve it (got %q)", out[0].Envelope.Subject())
	}
	if env.Subject() != "" {
		t.Fatalf("source envelope Subject must not be mutated, got %q", env.Subject())
	}
	if !del.IsAcked() {
		t.Fatal("delivery should be acked")
	}
}

// TestRouteRunner_DirectHold_ResolverError_Rejected verifies rejected resolve skips send, DLQs, and acks.
func TestRouteRunner_DirectHold_ResolverError_Rejected(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Resolver = &FakeResolver{
			ResolveErr: shared.NewBridgeError("NO_MATCH", shared.ErrorRejected, "no binding"),
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-reject"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "DLQ and ack on rejected resolve", func() bool {
		return del.IsAcked() && dlqStore.Count() == 1 && sender.SentCount() == 0
	})

	if sender.SentCount() != 0 {
		t.Fatal("should not send when resolver rejects")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
	if !del.IsAcked() {
		t.Fatal("should ack after DLQ")
	}
}

// TestRouteRunner_DirectHold_ResolverError_Transient verifies transient resolve errors retry without sending.
func TestRouteRunner_DirectHold_ResolverError_Transient(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Resolver = &FakeResolver{
			ResolveErr: shared.ErrUnavailable,
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-resolve"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "retry on transient resolver error", del.IsRetried)

	if sender.SentCount() != 0 {
		t.Fatal("should not send when resolver returns transient error")
	}
	if !del.IsRetried() {
		t.Fatal("should retry on transient resolver error")
	}
}

// TestRouteRunner_DirectHold_ResolverHeaders verifies dispatch plan headers merge with envelope headers on send.
func TestRouteRunner_DirectHold_ResolverHeaders(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{
					BindingID: "bind-hdr",
					Address:   "topic/resolved",
					Headers:   map[string]any{"qos": 1, "retain": true},
				},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-hdrs", Headers: map[string]any{"custom": "value"}})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "send with resolver headers", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", sender.SentCount())
	}
	sent := sender.GetSent()[0]
	out := sender.GetOutbound()[0]
	if out.Address != "topic/resolved" {
		t.Fatalf("expected Address topic/resolved, got %q", out.Address)
	}
	if sent.Subject() != "" {
		t.Fatalf("source envelope had no Subject; outbound must preserve it (got %q)", sent.Subject())
	}
	if sent.Headers()["qos"] != 1 {
		t.Fatalf("expected dispatch header qos=1, got %v", sent.Headers()["qos"])
	}
	if sent.Headers()["custom"] != "value" {
		t.Fatal("custom header should be preserved")
	}
}

// TestRouteRunner_SharedOutbox_FanOut verifies resolver fan-out writes one outbox record per plan and acks.
func TestRouteRunner_SharedOutbox_FanOut(t *testing.T) {
	receiver, _, _, outbox, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.Bindings = []routing.DestinationBinding{
			{ID: "bind-a", SessionID: "sess-a"},
			{ID: "bind-b", SessionID: "sess-b"},
		}
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "bind-a", Address: "topic/a"},
				{BindingID: "bind-b", Address: "topic/b"},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-fanout"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "fan-out outbox and ack", func() bool {
		return outbox.RecordCount() == 2 && del.IsAcked()
	})

	if outbox.RecordCount() != 2 {
		t.Fatalf("expected 2 outbox records for fan-out, got %d", outbox.RecordCount())
	}
	if !del.IsAcked() {
		t.Fatal("source should be acked after outbox persist")
	}
}

// TestRouteRunner_SharedOutbox_ResolverError_Rejected verifies rejected resolve with shared outbox DLQs and acks.
func TestRouteRunner_SharedOutbox_ResolverError_Rejected(t *testing.T) {
	receiver, _, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.Resolver = &FakeResolver{
			ResolveErr: shared.NewBridgeError("NO_MATCH", shared.ErrorRejected, "no binding"),
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-outbox-reject"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "DLQ and ack on outbox rejected resolve", func() bool {
		return dlqStore.Count() == 1 && del.IsAcked()
	})

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
	if !del.IsAcked() {
		t.Fatal("should ack after DLQ on rejected resolve error")
	}
}

// TestRouteRunner_Backpressure verifies MaxInFlight blocks a second emit until the in-flight send completes.
//
// Scenario: first send blocks on a hook; second emit must not finish until the first is released.
func TestRouteRunner_Backpressure(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.MaxInFlight = 1
	})

	blocked := make(chan struct{})
	released := make(chan struct{})
	sender.SendFn = func(env *messaging.Envelope) error {
		if env.ID() == "msg-block" {
			close(blocked)
			<-released
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del1 := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-block"}))
	go func() { _ = receiver.Emit(ctx, del1) }()

	<-blocked

	del2 := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-queued"}))
	emitDone := make(chan struct{})
	go func() {
		_ = receiver.Emit(ctx, del2)
		close(emitDone)
	}()

	select {
	case <-emitDone:
		t.Fatal("second delivery should be blocked by backpressure")
	case <-time.After(100 * time.Millisecond):
	}

	close(released)

	select {
	case <-emitDone:
	case <-time.After(time.Second):
		t.Fatal("second delivery should complete after first is released")
	}
}

// TestRouteRunner_MQTTToSQS_DirectHold verifies the reverse direction:
// an MQTT source receiver emits a delivery that flows through the
// processor chain and is sent to an SQS-style sender via direct_hold.
func TestRouteRunner_MQTTToSQS_DirectHold(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "sqs-bind", Address: "arn:aws:sqs:eu-west-1:123456789:orders"},
			},
		}
		cfg.Processors = []ports.Processor{
			&FakeProcessor{
				NameVal: "mqtt-to-sqs-enricher",
				ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
					env.SetHeader("source-transport", "mqtt")
					return next(ctx, env)
				},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "mqtt-ingress-1",
		Subject: "factory/a/telemetry",
		Payload: []byte(`{"temp":22.5}`),
		Headers: map[string]any{"qos": 1},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "MQTT to SQS send and ack", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", sender.SentCount())
	}
	sent := sender.GetSent()[0]
	out := sender.GetOutbound()[0]
	if out.Address != "arn:aws:sqs:eu-west-1:123456789:orders" {
		t.Fatalf("expected SQS Address, got %q", out.Address)
	}
	if sent.Subject() != "factory/a/telemetry" {
		t.Fatalf("logical Subject must be preserved on outbound, got %q", sent.Subject())
	}
	if env.Subject() != "factory/a/telemetry" {
		t.Fatalf("source envelope Subject must not be mutated, got %q", env.Subject())
	}
	if sent.Headers()["source-transport"] != "mqtt" {
		t.Fatal("processor should have set source-transport header")
	}
	if !del.IsAcked() {
		t.Fatal("delivery should be acked after successful send")
	}
}

// TestRouteRunner_MQTTToSQS_SharedOutbox verifies the reverse direction
// with shared_outbox delivery mode: MQTT source -> outbox persist -> SQS send.
func TestRouteRunner_MQTTToSQS_SharedOutbox(t *testing.T) {
	receiver, _, _, outbox, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.Resolver = &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "sqs-bind", Address: "arn:aws:sqs:eu-west-1:123456789:events"},
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "mqtt-to-sqs-outbox-1",
		Subject: "sensors/temp",
		Payload: []byte(`{"temp":19.3}`),
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "MQTT to SQS outbox persist and ack", func() bool {
		return outbox.RecordCount() == 1 && del.IsAcked()
	})

	if outbox.RecordCount() != 1 {
		t.Fatalf("expected 1 outbox record, got %d", outbox.RecordCount())
	}
	if !del.IsAcked() {
		t.Fatal("source should be acked after outbox persist")
	}
}
