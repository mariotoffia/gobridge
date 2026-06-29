package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestRouteRunner_EmitsE2ELatency verifies DeliveryE2ELatency is recorded with route_id for a direct_hold runner.
func TestRouteRunner_EmitsE2ELatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	receiver := NewFakeReceiver()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-e2e",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Metrics:  rec,
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1", Payload: []byte("data"), ExpiresAt: time.Now().Add(time.Hour)})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	cancel()

	timers := rec.FindEntries(shared.MetricDeliveryE2ELatency)
	if len(timers) == 0 {
		t.Fatal("expected at least 1 DeliveryE2ELatency timer")
	}
	found := false
	for _, tag := range timers[0].Tags {
		if tag.Key == shared.TagKeyRouteID && tag.Value == "route-e2e" {
			found = true
		}
	}
	if !found {
		t.Error("expected route_id tag on E2E latency metric")
	}
}

// TestRouteRunner_EmitsDLQEntries verifies DLQEntries is emitted when send fails with a permanent error.
func TestRouteRunner_EmitsDLQEntries(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	sender.SendErr = shared.NewBridgeError("PERM", shared.ErrorPermanent, "permanent failure")
	receiver := NewFakeReceiver()
	dlqStore := NewFakeDLQStore()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-dlq",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Metrics:  rec,
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-dlq", Payload: []byte("data"), ExpiresAt: time.Now().Add(time.Hour)})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	cancel()

	dlqCounters := rec.FindEntries(shared.MetricDLQEntries)
	if len(dlqCounters) == 0 {
		t.Fatal("expected DLQEntries counter emission")
	}
}

// TestOutboxDrainer_EmitsDrainLatency verifies OutboxDrainLatency and OutboxCompletions are emitted after a drain cycle.
func TestOutboxDrainer_EmitsDrainLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	sender := NewFakeSender()

	token, _ := lease.Acquire(context.Background(), "sess-1", "owner-1", 30*time.Second, nil)

	records := []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "r1", RouteID: "route-drain", EnvelopeID: "e1", BindingID: "b1",
		SessionID: "sess-1", Status: persistence.OutboxPending,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("data")}),
	})}
	_ = outbox.Persist(context.Background(), records)

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		RouteID:        "route-drain",
		PartitionKey:   persistence.OutboxPartitionKey("sess-1", "b1"),
		LeaseID:        "sess-1",
		Policy:         routing.RoutePolicy{}.WithDefaults(),
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		Metrics:        rec,
		TokenFn:        func() (persistence.LeaseToken, bool) { return token, true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(ctx)

	timers := rec.FindEntries(shared.MetricOutboxDrainLatency)
	if len(timers) == 0 {
		t.Fatal("expected OutboxDrainLatency timer emission")
	}

	completions := rec.FindEntries(shared.MetricOutboxCompletions)
	if len(completions) == 0 {
		t.Fatal("expected OutboxCompletions counter emission")
	}
}

// TestOutboxDrainer_EmitsExpiredBeforeSend verifies OutboxExpiredBeforeSend is emitted for expired records.
func TestOutboxDrainer_EmitsExpiredBeforeSend(t *testing.T) {
	rec := &ports.RecordingExporter{}
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	sender := NewFakeSender()

	token, _ := lease.Acquire(context.Background(), "s1", "owner-1", 30*time.Second, nil)

	records := []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "r-exp", RouteID: "route-exp", EnvelopeID: "e-exp", BindingID: "b1",
		SessionID: "s1", Status: persistence.OutboxPending,
		Envelope: func() messaging.Envelope {
			e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-exp", Payload: []byte("data")})
			_ = e.SetExpiry(time.Now().Add(-time.Hour))
			return *e
		}(),
		ExpiresAt: time.Now().Add(-time.Hour),
	})}
	_ = outbox.Persist(context.Background(), records)

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		RouteID:        "route-exp",
		PartitionKey:   persistence.OutboxPartitionKey("s1", "b1"),
		LeaseID:        "s1",
		Policy:         routing.RoutePolicy{OnExpired: routing.ExpiredDLQ}.WithDefaults(),
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		Metrics:        rec,
		TokenFn:        func() (persistence.LeaseToken, bool) { return token, true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(ctx)

	expired := rec.FindEntries(shared.MetricOutboxExpiredBeforeSend)
	if len(expired) == 0 {
		t.Fatal("expected OutboxExpiredBeforeSend counter emission")
	}
}

// TestSessionManager_EmitsLeaseMetrics verifies lease acquire and renew latency metrics during an exclusive sess run.
func TestSessionManager_EmitsLeaseMetrics(t *testing.T) {
	rec := &ports.RecordingExporter{}
	lease := NewFakeLeaseStore()
	sess := NewFakeSession()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID:     "sess-metric",
			Exclusive:     true,
			LeaseTTL:      30 * time.Second,
			RenewInterval: 50 * time.Millisecond,
			RenewJitter:   0,
			MaxRenewFails: 3,
			StepDownGrace: 10 * time.Millisecond,
		},
		sess, lease, "owner-1", nil,
	)
	mgr.SetMetrics(rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = mgr.Run(ctx)
		close(done)
	}()

	waitFor(t, 2*time.Second, "LeaseAcquireLatency emitted", func() bool {
		return len(rec.FindEntries(shared.MetricLeaseAcquireLatency)) > 0
	})
	waitFor(t, 2*time.Second, "LeaseRenewLatency emitted", func() bool {
		return len(rec.FindEntries(shared.MetricLeaseRenewLatency)) > 0
	})

	cancel()
	<-done
}

// TestSessionManager_EmitsReconnectMetric verifies MQTTReconnects counts a second SessionConnected after the first.
func TestSessionManager_EmitsReconnectMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewFakeSession()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID: "sess-reconnect",
			Exclusive: false,
		},
		sess, nil, "owner-1", nil,
	)
	mgr.SetMetrics(rec)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond) // OTHER: simulated event timeline spacing
		// First connect - should NOT count as reconnect.
		sess.PushEvent(ports.SessionEvent{
			Type:      ports.SessionConnected,
			Timestamp: time.Now(),
		})
		time.Sleep(50 * time.Millisecond) // OTHER: simulated event timeline spacing
		// Second connect - should count as reconnect.
		sess.PushEvent(ports.SessionEvent{
			Type:      ports.SessionConnected,
			Timestamp: time.Now(),
		})
		time.Sleep(50 * time.Millisecond) // OTHER: simulated event timeline spacing
		cancel()
	}()

	_ = mgr.Run(ctx)

	reconnects := rec.FindEntries(shared.MetricMQTTReconnects)
	if len(reconnects) != 1 {
		t.Fatalf("expected 1 MQTTReconnects counter (not counting initial connect), got %d", len(reconnects))
	}
}
