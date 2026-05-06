package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

func TestRouteRunner_E2ELatencyUsesInjectedClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	sender.SendFn = func(*domain.Envelope) error {
		fake.Advance(42 * time.Millisecond)
		return nil
	}
	receiver := NewFakeReceiver()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "route-clocked-latency",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Metrics:  rec,
		Clock:    fake,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{
		ID:        "msg-clocked-latency",
		ExpiresAt: fake.Now().Add(time.Hour),
	})
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "clocked latency timer", func() bool {
		return del.IsAcked() && len(rec.FindEntries(shared.MetricDeliveryE2ELatency)) == 1
	})

	entries := rec.FindEntries(shared.MetricDeliveryE2ELatency)
	if got := entries[0].Duration; got != 42*time.Millisecond {
		t.Fatalf("expected E2E latency from injected clock, got %s", got)
	}
}

func TestRouteRunner_SharedOutboxCreatedAtUsesInjectedClock(t *testing.T) {
	createdAt := time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC)
	fake := clocktest.NewAt(createdAt)
	var (
		persistMu sync.Mutex
		persisted []domain.OutboxRecord
	)
	outbox := NewFakeOutboxStore()
	outbox.PersistFn = func(records []domain.OutboxRecord) error {
		persistMu.Lock()
		defer persistMu.Unlock()
		persisted = append(persisted, records...)
		return nil
	}
	receiver := NewFakeReceiver()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:     "route-clocked-outbox",
		Policy:      domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Receiver:    receiver,
		Sender:      NewFakeSender(),
		OutboxStore: outbox,
		Resolver: &FakeResolver{Plans: []domain.DispatchPlan{{
			BindingID: "bind-clocked",
			Address:   "topic/clocked",
		}}},
		Clock: fake,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{
		ID:        "msg-clocked-outbox",
		ExpiresAt: fake.Now().Add(time.Hour),
	})
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "clocked outbox record", func() bool {
		persistMu.Lock()
		count := len(persisted)
		persistMu.Unlock()
		return del.IsAcked() && count == 1
	})

	persistMu.Lock()
	got := persisted[0].CreatedAt
	persistMu.Unlock()
	if !got.Equal(createdAt) {
		t.Fatalf("expected outbox CreatedAt from injected clock %s, got %s", createdAt, got)
	}
}
