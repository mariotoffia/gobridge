package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════════
// S13-gap: RouteRunner Child Goroutine Panic Recovery
//
// Validates that panics in delivery-processing goroutines are recovered
// without crashing the process, that the delivery is retried, metrics
// are emitted, and semaphore slots are released.
//
//   ┌────────────┐  panic!  ┌───────────┐
//   │ go func()  │─────────▶│ recover() │──▶ log + metric + retry
//   │ process    │          │  defer    │
//   │ Delivery   │          └───────────┘
//   └────────────┘               │
//                                ▼
//                         ┌─────────────┐
//                         │ releaseSlots │
//                         │ wg.Done()   │
//                         └─────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                    │ Type     │
// ├──────┼────────────────────────────────────────────────┼──────────┤
// │ T1   │ Processor panic does not crash                 │ unit     │
// │ T2   │ Processor panic retries delivery               │ unit     │
// │ T3   │ Sender panic does not crash                    │ unit     │
// │ T4   │ Sender panic retries delivery                  │ unit     │
// │ T5   │ Panic emits DeliveryPanics metric              │ unit     │
// │ T6   │ Panic releases semaphore slots                 │ unit     │
// │ T7   │ Panic on one msg doesn't affect others         │ unit     │
// │ T8   │ Retry failure after panic doesn't crash        │ unit     │
// │ T9   │ Retry panicking after panic doesn't crash      │ unit     │
// └──────┴────────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestRouteRunner_ProcessorPanic_DoesNotCrash validates that a panicking
// processor does not crash the process and Run completes gracefully.
// Processor panics are classified Permanent (ErrProcessorPanic) and
// routed to DLQ, then the source delivery is acked.
func TestRouteRunner_ProcessorPanic_DoesNotCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-proc-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
					panic("processor exploded")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-proc-panic"}))
	_ = receiver.Emit(ctx, del)

	// With no DLQ store, Permanent errors fall through to ack (drop).
	waitFor(t, 2*time.Second, "delivery acked after processor panic", del.IsAcked)
	if del.IsRetried() {
		t.Fatal("processor panic must not trigger retry (it is Permanent)")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRouteRunner_ProcessorPanic_RoutesToDLQ validates that after a
// processor panic, the envelope is written to the DLQ with an error
// chain satisfying errors.Is(..., ErrProcessorPanic) and the source
// delivery is acked.
func TestRouteRunner_ProcessorPanic_RoutesToDLQ(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-proc-retry",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
					panic("processor exploded")
				},
			},
		},
	})

	ctx := t.Context()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-proc-retry"}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked after processor panic", del.IsAcked)

	if del.IsRetried() {
		t.Fatal("processor panic must not retry -- Permanent goes to DLQ")
	}

	dlqStore.mu.Lock()
	entries := append([]routing.DLQEntry(nil), dlqStore.Entries...)
	dlqStore.mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0].LastError(), "processor panicked") {
		t.Fatalf("DLQ LastError should mention processor panic, got %q", entries[0].LastError())
	}
}

// TestRouteRunner_SenderPanic_DoesNotCrash validates that a panicking
// sender does not crash the process.
func TestRouteRunner_SenderPanic_DoesNotCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendFn = func(_ *messaging.Envelope) error {
		panic("sender exploded")
	}

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-send-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-send-panic"}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after sender panic", del.IsRetried)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRouteRunner_SenderPanic_RetriesDelivery validates that after
// recovering from a sender panic, the delivery is retried.
func TestRouteRunner_SenderPanic_RetriesDelivery(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendFn = func(_ *messaging.Envelope) error {
		panic("sender exploded")
	}

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-send-retry",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx := t.Context()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-send-retry"}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after sender panic", del.IsRetried)

	if del.IsAcked() {
		t.Fatal("delivery should not be acked after sender panic")
	}
	if !del.IsRetried() {
		t.Fatal("delivery should be retried after sender panic recovery")
	}
}

// TestRouteRunner_ProcessorPanic_EmitsMetric validates that a processor
// panic emits a MetricProcessorPanics counter with a route_id tag.
func TestRouteRunner_ProcessorPanic_EmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-metric-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Metrics:  rec,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
					panic("metric test panic")
				},
			},
		},
	})

	ctx := t.Context()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-metric-panic"}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked after panic", del.IsAcked)

	counters := rec.FindEntries(shared.MetricProcessorPanics)
	if len(counters) < 1 {
		t.Fatalf("expected at least 1 ProcessorPanics counter, got %d", len(counters))
	}
	if counters[0].IValue != 1 {
		t.Fatalf("expected counter value 1, got %d", counters[0].IValue)
	}

	foundRouteTag := false
	for _, tag := range counters[0].Tags {
		if tag.Key == shared.TagKeyRouteID && tag.Value == "panic-metric-route" {
			foundRouteTag = true
		}
	}
	if !foundRouteTag {
		t.Fatal("ProcessorPanics counter should have route_id tag")
	}
}

// TestRouteRunner_DeliveryPanic_SlotsReleased validates that after panic
// recovery, semaphore slots are released so subsequent deliveries are
// not blocked. MaxInFlight=1: first delivery panics, second succeeds.
func TestRouteRunner_DeliveryPanic_SlotsReleased(t *testing.T) {
	var callCount int64
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-slot-route",
		Policy:   routing.RoutePolicy{MaxInFlight: 1, DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "conditional-panicker",
				ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
					n := atomic.AddInt64(&callCount, 1)
					if n == 1 {
						panic("first call panics")
					}
					return next(ctx, env)
				},
			},
		},
	})

	ctx := t.Context()

	go func() { _ = runner.Run(ctx) }()

	del1 := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-panic-slot"}))
	_ = receiver.Emit(ctx, del1)
	waitFor(t, 2*time.Second, "first delivery acked (panic routed to DLQ/drop)", del1.IsAcked)

	del2 := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-ok-slot"}))
	_ = receiver.Emit(ctx, del2)
	waitFor(t, 2*time.Second, "second delivery acked (slot released)", del2.IsAcked)

	if !del1.IsAcked() {
		t.Fatal("first delivery should be acked after panic (Permanent → DLQ/drop)")
	}
	if !del2.IsAcked() {
		t.Fatal("second delivery should be acked — slot must have been released")
	}
}

// TestRouteRunner_DeliveryPanic_OtherMessagesUnaffected validates that
// a panic on one delivery does not affect concurrent deliveries.
// MaxInFlight=5: one message panics, four others succeed normally.
func TestRouteRunner_DeliveryPanic_OtherMessagesUnaffected(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendFn = func(env *messaging.Envelope) error {
		if env.ID() == "msg-panic-concurrent" {
			panic("targeted panic")
		}
		return nil
	}

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-concurrent-route",
		Policy:   routing.RoutePolicy{MaxInFlight: 5, DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx := t.Context()

	go func() { _ = runner.Run(ctx) }()

	panicDel := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-panic-concurrent"}))
	_ = receiver.Emit(ctx, panicDel)

	okDels := make([]*FakeDelivery, 4)
	for i := range okDels {
		okDels[i] = NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("msg-ok-%d", i),
			Payload: []byte("data"),
		}))
		_ = receiver.Emit(ctx, okDels[i])
	}

	waitFor(t, 2*time.Second, "panicking delivery retried", panicDel.IsRetried)
	for i, del := range okDels {
		waitFor(t, 2*time.Second, fmt.Sprintf("ok delivery %d acked", i), del.IsAcked)
	}

	if !panicDel.IsRetried() {
		t.Fatal("panicking delivery should be retried")
	}
	for i, del := range okDels {
		if !del.IsAcked() {
			t.Fatalf("ok delivery %d should be acked", i)
		}
	}
}

// TestRouteRunner_ProcessorPanic_DLQWriteFails_NoCrash validates that
// if the DLQ store write fails for a panic-induced entry, the runtime
// falls back to retry and the process does not crash.
func TestRouteRunner_ProcessorPanic_DLQWriteFails_NoCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	dlqStore.WriteErr = fmt.Errorf("simulated DLQ write failure")

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-retry-fail",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
					panic("retry will fail too")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-fail"}))
	del.RetryFnErr = fmt.Errorf("simulated retry transport failure")
	_ = receiver.Emit(ctx, del)

	// When DLQ write fails, handleProcessorError falls back to retry.
	waitFor(t, 2*time.Second, "delivery retry attempted after DLQ write failure", del.IsRetried)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel -- possible goroutine leak")
	}
}

// TestRouteRunner_ProcessorPanic_RetryPanics_NoProcessCrash validates
// that a panicking retry path (triggered by a DLQ write failure on a
// panic-induced entry) does not crash the process: the outer delivery
// goroutine recover catches the nested panic.
func TestRouteRunner_ProcessorPanic_RetryPanics_NoProcessCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	dlqStore.WriteErr = fmt.Errorf("forced DLQ failure")

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "panic-retry-panic",
		Policy:   routing.RoutePolicy{MaxInFlight: 1, DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
					panic("processor panic")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := &panickingRetryDelivery{
		env: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-panics"}),
	}
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "panicking retry delivery processed", func() bool {
		return del.retryAttempted.Load()
	})

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel -- nested panic leaked")
	}
}

// panickingRetryDelivery is a delivery whose Retry method panics,
// simulating a broken transport layer inside the recovery handler.
type panickingRetryDelivery struct {
	env            *messaging.Envelope
	retryAttempted atomic.Bool
}

func (d *panickingRetryDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *panickingRetryDelivery) Ack(_ context.Context) error   { return nil }
func (d *panickingRetryDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	d.retryAttempted.Store(true)
	panic("retry itself panicked")
}
func (d *panickingRetryDelivery) Extend(_ context.Context, _ time.Time) error { return nil }
