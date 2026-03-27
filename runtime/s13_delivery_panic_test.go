package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
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
func TestRouteRunner_ProcessorPanic_DoesNotCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-proc-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
					panic("processor exploded")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-proc-panic"})
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after processor panic", del.IsRetried)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRouteRunner_ProcessorPanic_RetriesDelivery validates that after
// recovering from a processor panic, the delivery is retried with a
// reason containing "panic recovered".
func TestRouteRunner_ProcessorPanic_RetriesDelivery(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-proc-retry",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
					panic("processor exploded")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-proc-retry"})
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after processor panic", del.IsRetried)

	if del.IsAcked() {
		t.Fatal("delivery should not be acked after panic")
	}
	if !del.IsRetried() {
		t.Fatal("delivery should be retried after panic recovery")
	}
	del.mu.Lock()
	reason := del.RetryErr
	del.mu.Unlock()
	if reason == nil {
		t.Fatal("retry reason must not be nil")
	}
	if !strings.Contains(reason.Error(), "panic recovered") {
		t.Fatalf("retry reason should contain 'panic recovered', got: %s", reason)
	}
}

// TestRouteRunner_SenderPanic_DoesNotCrash validates that a panicking
// sender does not crash the process.
func TestRouteRunner_SenderPanic_DoesNotCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendFn = func(_ *domain.Envelope) error {
		panic("sender exploded")
	}

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-send-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-send-panic"})
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
	sender.SendFn = func(_ *domain.Envelope) error {
		panic("sender exploded")
	}

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-send-retry",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-send-retry"})
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after sender panic", del.IsRetried)

	if del.IsAcked() {
		t.Fatal("delivery should not be acked after sender panic")
	}
	if !del.IsRetried() {
		t.Fatal("delivery should be retried after sender panic recovery")
	}
}

// TestRouteRunner_DeliveryPanic_EmitsMetric validates that panic recovery
// emits a MetricDeliveryPanics counter with a route_id tag.
func TestRouteRunner_DeliveryPanic_EmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-metric-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Metrics:  rec,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
					panic("metric test panic")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-metric-panic"})
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried after panic", del.IsRetried)

	counters := rec.FindEntries(domain.MetricDeliveryPanics)
	if len(counters) != 1 {
		t.Fatalf("expected 1 DeliveryPanics counter, got %d", len(counters))
	}
	if counters[0].IValue != 1 {
		t.Fatalf("expected counter value 1, got %d", counters[0].IValue)
	}

	foundRouteTag := false
	for _, tag := range counters[0].Tags {
		if tag.Key == domain.TagKeyRouteID && tag.Value == "panic-metric-route" {
			foundRouteTag = true
		}
	}
	if !foundRouteTag {
		t.Fatal("DeliveryPanics counter should have route_id tag")
	}
}

// TestRouteRunner_DeliveryPanic_SlotsReleased validates that after panic
// recovery, semaphore slots are released so subsequent deliveries are
// not blocked. MaxInFlight=1: first delivery panics, second succeeds.
func TestRouteRunner_DeliveryPanic_SlotsReleased(t *testing.T) {
	var callCount int64
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-slot-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 1, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "conditional-panicker",
				ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
					n := atomic.AddInt64(&callCount, 1)
					if n == 1 {
						panic("first call panics")
					}
					return next(ctx, env)
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del1 := NewFakeDelivery(&domain.Envelope{ID: "msg-panic-slot"})
	_ = receiver.Emit(ctx, del1)
	waitFor(t, 2*time.Second, "first delivery retried (panic)", del1.IsRetried)

	del2 := NewFakeDelivery(&domain.Envelope{ID: "msg-ok-slot"})
	_ = receiver.Emit(ctx, del2)
	waitFor(t, 2*time.Second, "second delivery acked (slot released)", del2.IsAcked)

	if !del1.IsRetried() {
		t.Fatal("first delivery should be retried after panic")
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
	sender.SendFn = func(env *domain.Envelope) error {
		if env.ID == "msg-panic-concurrent" {
			panic("targeted panic")
		}
		return nil
	}

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-concurrent-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 5, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	panicDel := NewFakeDelivery(&domain.Envelope{ID: "msg-panic-concurrent"})
	_ = receiver.Emit(ctx, panicDel)

	okDels := make([]*FakeDelivery, 4)
	for i := range okDels {
		okDels[i] = NewFakeDelivery(&domain.Envelope{
			ID:      fmt.Sprintf("msg-ok-%d", i),
			Payload: []byte("data"),
		})
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

// TestRouteRunner_DeliveryPanic_RetryFails_NoSecondPanic validates that
// if del.Retry() returns an error after panic recovery, no crash occurs.
func TestRouteRunner_DeliveryPanic_RetryFails_NoSecondPanic(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-retry-fail",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
					panic("retry will fail too")
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-retry-fail"})
	del.RetryFnErr = fmt.Errorf("simulated retry transport failure")
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retry attempted despite failure", del.IsRetried)

	if !del.IsRetried() {
		t.Fatal("del.Retry() should have been called even though it returns error")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel — possible goroutine leak")
	}
}

// TestRouteRunner_DeliveryPanic_RetryPanics_NoProcessCrash validates
// that if del.Retry() itself panics inside the recovery handler, the
// nested recover catches it and the process does not crash.
func TestRouteRunner_DeliveryPanic_RetryPanics_NoProcessCrash(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "panic-retry-panic",
		Policy:   domain.RoutePolicy{MaxInFlight: 1, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Processors: []ports.Processor{
			&FakeProcessor{
				NameVal: "panicker",
				ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
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
		env: &domain.Envelope{ID: "msg-retry-panics"},
	}
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "panicking retry delivery processed", func() bool {
		return del.retryAttempted.Load()
	})

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel — nested panic leaked")
	}
}

// panickingRetryDelivery is a delivery whose Retry method panics,
// simulating a broken transport layer inside the recovery handler.
type panickingRetryDelivery struct {
	env            *domain.Envelope
	retryAttempted atomic.Bool
}

func (d *panickingRetryDelivery) Envelope() *domain.Envelope { return d.env }
func (d *panickingRetryDelivery) Ack(_ context.Context) error { return nil }
func (d *panickingRetryDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	d.retryAttempted.Store(true)
	panic("retry itself panicked")
}
func (d *panickingRetryDelivery) Extend(_ context.Context, _ time.Time) error { return nil }
