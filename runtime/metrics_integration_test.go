package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestRouteRunner_EmitsE2ELatency verifies DeliveryE2ELatency is recorded with route_id for a direct_hold runner.
func TestRouteRunner_EmitsE2ELatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	receiver := NewFakeReceiver()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "route-e2e",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Metrics:  rec,
		Bindings: []domain.DestinationBinding{{ID: "b1"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()

	env := &domain.Envelope{ID: "msg-1", Payload: []byte("data"), ExpiresAt: time.Now().Add(time.Hour)}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	cancel()

	timers := rec.FindEntries(domain.MetricDeliveryE2ELatency)
	if len(timers) == 0 {
		t.Fatal("expected at least 1 DeliveryE2ELatency timer")
	}
	found := false
	for _, tag := range timers[0].Tags {
		if tag.Key == domain.TagKeyRouteID && tag.Value == "route-e2e" {
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
	sender.SendErr = domain.NewBridgeError("PERM", domain.ErrorPermanent, "permanent failure")
	receiver := NewFakeReceiver()
	dlqStore := NewFakeDLQStore()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "route-dlq",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(dlqStore),
		Metrics:  rec,
		Bindings: []domain.DestinationBinding{{ID: "b1"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()

	env := &domain.Envelope{ID: "msg-dlq", Payload: []byte("data"), ExpiresAt: time.Now().Add(time.Hour)}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	cancel()

	dlqCounters := rec.FindEntries(domain.MetricDLQEntries)
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

	token, _ := lease.Acquire(context.Background(), "session-1", "owner-1", 30*time.Second)

	records := []domain.OutboxRecord{{
		ID: "r1", RouteID: "route-drain", EnvelopeID: "e1", BindingID: "b1",
		SessionID: "session-1", Status: domain.OutboxPending,
		Envelope: domain.Envelope{ID: "e1", Payload: []byte("data")},
	}}
	_ = outbox.Persist(context.Background(), records)

	drainer := runtime.NewOutboxDrainerFromConfig(runtime.OutboxDrainerConfig{
		OutboxStore:   outbox,
		LeaseStore:    lease,
		Sender:        sender,
		RouteID:       "route-drain",
		PartitionKey:  domain.OutboxPartitionKey("session-1", "b1"),
		LeaseID:       "session-1",
		OwnerID:       "owner-1",
		Policy:    domain.RoutePolicy{}.WithDefaults(),
		Strategy:  domain.NewFixedPoll(50 * time.Millisecond),
		BatchSize: 10,
		Metrics:   rec,
		TokenFn:   func() (domain.LeaseToken, bool) { return token, true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(ctx)

	timers := rec.FindEntries(domain.MetricOutboxDrainLatency)
	if len(timers) == 0 {
		t.Fatal("expected OutboxDrainLatency timer emission")
	}

	completions := rec.FindEntries(domain.MetricOutboxCompletions)
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

	token, _ := lease.Acquire(context.Background(), "s1", "owner-1", 30*time.Second)

	records := []domain.OutboxRecord{{
		ID: "r-exp", RouteID: "route-exp", EnvelopeID: "e-exp", BindingID: "b1",
		SessionID: "s1", Status: domain.OutboxPending,
		Envelope: domain.Envelope{
			ID:        "e-exp",
			Payload:   []byte("data"),
			ExpiresAt: time.Now().Add(-time.Hour),
		},
		ExpiresAt: time.Now().Add(-time.Hour),
	}}
	_ = outbox.Persist(context.Background(), records)

	drainer := runtime.NewOutboxDrainerFromConfig(runtime.OutboxDrainerConfig{
		OutboxStore:   outbox,
		LeaseStore:    lease,
		Sender:        sender,
		RouteID:       "route-exp",
		PartitionKey:  domain.OutboxPartitionKey("s1", "b1"),
		LeaseID:       "s1",
		OwnerID:       "owner-1",
		Policy:    domain.RoutePolicy{OnExpired: domain.ExpiredDLQ}.WithDefaults(),
		Strategy:  domain.NewFixedPoll(50 * time.Millisecond),
		BatchSize: 10,
		Metrics:   rec,
		TokenFn:   func() (domain.LeaseToken, bool) { return token, true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(ctx)

	expired := rec.FindEntries(domain.MetricOutboxExpiredBeforeSend)
	if len(expired) == 0 {
		t.Fatal("expected OutboxExpiredBeforeSend counter emission")
	}
}

// TestSessionManager_EmitsLeaseMetrics verifies lease acquire and renew latency metrics during an exclusive session run.
func TestSessionManager_EmitsLeaseMetrics(t *testing.T) {
	rec := &ports.RecordingExporter{}
	lease := NewFakeLeaseStore()
	session := NewFakeSession()

	mgr := runtime.NewSessionManagerFromConfig(
		runtime.SessionConfig{
			SessionID:     "sess-metric",
			Exclusive:     true,
			LeaseTTL:      30 * time.Second,
			RenewInterval: 50 * time.Millisecond,
			RenewJitter:   0,
			MaxRenewFails: 3,
			StepDownGrace: 10 * time.Millisecond,
		},
		session, lease, "owner-1", nil,
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
		return len(rec.FindEntries(domain.MetricLeaseAcquireLatency)) > 0
	})
	waitFor(t, 2*time.Second, "LeaseRenewLatency emitted", func() bool {
		return len(rec.FindEntries(domain.MetricLeaseRenewLatency)) > 0
	})

	cancel()
	<-done
}

// TestSessionManager_EmitsReconnectMetric verifies MQTTReconnects counts a second SessionConnected after the first.
func TestSessionManager_EmitsReconnectMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	session := NewFakeSession()

	mgr := runtime.NewSessionManagerFromConfig(
		runtime.SessionConfig{
			SessionID: "sess-reconnect",
			Exclusive: false,
		},
		session, nil, "owner-1", nil,
	)
	mgr.SetMetrics(rec)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		// First connect - should NOT count as reconnect.
		session.PushEvent(ports.SessionEvent{
			Type:      ports.SessionConnected,
			Timestamp: time.Now(),
		})
		time.Sleep(50 * time.Millisecond)
		// Second connect - should count as reconnect.
		session.PushEvent(ports.SessionEvent{
			Type:      ports.SessionConnected,
			Timestamp: time.Now(),
		})
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_ = mgr.Run(ctx)

	reconnects := rec.FindEntries(domain.MetricMQTTReconnects)
	if len(reconnects) != 1 {
		t.Fatalf("expected 1 MQTTReconnects counter (not counting initial connect), got %d", len(reconnects))
	}
}
