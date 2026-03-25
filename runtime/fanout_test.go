package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// TestFanOut_SingleRouteMultipleSessions verifies that one source message
// resolved to multiple dispatch plans targeting different MQTT sessions is
// persisted to the outbox and drained by the correct session-specific
// drainer using RegisterSessionSender.
func TestFanOut_SingleRouteMultipleSessions(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-fanout", outbox, lease, dlq)

	receiver := NewFakeReceiver()

	// Primary session/sender (factory A).
	senderA := NewTrackingSender("factory-a")
	sessionA := NewFakeSession()
	sessCfgA := fastSessionConfig("mqtt-factory-a")

	// Secondary session/sender (factory B) — registered via RegisterSessionSender.
	senderB := NewTrackingSender("factory-b")
	sessionB := NewFakeSession()
	sessCfgB := fastSessionConfig("mqtt-factory-b")

	if err := rt.RegisterSessionSender(sessCfgB, sessionB, senderB); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}

	resolver := &FakeResolver{
		Plans: []domain.DispatchPlan{
			{BindingID: "bind-factory-a", Address: "factory/a/orders/42"},
			{BindingID: "bind-factory-b", Address: "factory/b/orders/42"},
		},
	}

	cfg := goruntime.RouteConfig{
		ID: "fanout-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: resolver,
		Bindings: []domain.DestinationBinding{
			{ID: "bind-factory-a", SessionID: "mqtt-factory-a"},
			{ID: "bind-factory-b", SessionID: "mqtt-factory-b"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, senderA, sessionA, &sessCfgA); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	// Wait for both sessions to start.
	waitFor(t, 3*time.Second, "session A started", func() bool {
		return sessionA.IsStarted()
	})
	waitFor(t, 3*time.Second, "session B started", func() bool {
		return sessionB.IsStarted()
	})

	// Send a message that fans out to both sessions.
	env := &domain.Envelope{
		ID:      "fanout-msg-1",
		Payload: []byte("multi-factory-order"),
	}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	// Two outbox records should exist (one per dispatch plan).
	waitFor(t, time.Second, "2 outbox records", func() bool {
		return outbox.RecordCount() >= 2
	})

	// Both drainers should send their respective records.
	waitFor(t, 5*time.Second, "both senders sent", func() bool {
		return senderA.SentCount() >= 1 && senderB.SentCount() >= 1
	})

	sentA := senderA.GetSent()
	if sentA[0].Subject != "factory/a/orders/42" {
		t.Errorf("sender A: expected address factory/a/orders/42, got %q", sentA[0].Subject)
	}

	sentB := senderB.GetSent()
	if sentB[0].Subject != "factory/b/orders/42" {
		t.Errorf("sender B: expected address factory/b/orders/42, got %q", sentB[0].Subject)
	}

	// Both records should be completed.
	waitFor(t, 3*time.Second, "both completed", func() bool {
		return outbox.CompletedCount() >= 2
	})
}

// TestFanOut_PartialSessionAvailability verifies that if one session's
// drainer sends successfully but another session is unavailable, only
// the successful record is completed. The pending one remains for later.
func TestFanOut_PartialSessionAvailability(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-partial", outbox, lease, dlq)

	receiver := NewFakeReceiver()

	senderA := NewFakeSender()
	sessionA := NewFakeSession()
	sessCfgA := fastSessionConfig("mqtt-partial-a")

	// Sender B will fail all sends (simulating offline target).
	senderB := NewFakeSender()
	senderB.SendErr = domain.NewBridgeError("TARGET_DOWN", domain.ErrorTransient, "offline")
	sessionB := NewFakeSession()
	sessCfgB := fastSessionConfig("mqtt-partial-b")

	_ = rt.RegisterSessionSender(sessCfgB, sessionB, senderB)

	cfg := goruntime.RouteConfig{
		ID: "partial-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []domain.DispatchPlan{
				{BindingID: "ba", Address: "topic/a"},
				{BindingID: "bb", Address: "topic/b"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "ba", SessionID: "mqtt-partial-a"},
			{ID: "bb", SessionID: "mqtt-partial-b"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, senderA, sessionA, &sessCfgA)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 3*time.Second, "sessions started", func() bool {
		return sessionA.IsStarted() && sessionB.IsStarted()
	})

	env := &domain.Envelope{ID: "partial-msg-1", Payload: []byte("data")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "acked", func() bool {
		return del.IsAcked()
	})

	// Sender A should complete its record.
	waitFor(t, 3*time.Second, "sender A sent", func() bool {
		return senderA.SentCount() >= 1
	})

	// At least 1 completed (A's), B's record remains pending/claimed.
	waitFor(t, 2*time.Second, "A completed", func() bool {
		return outbox.CompletedCount() >= 1
	})

	// B's record is not completed because sends fail.
	if outbox.CompletedCount() > 1 {
		t.Error("B's record should not be completed while sender is failing")
	}

	// Now fix sender B.
	senderB.SetSendErr(nil)

	// B should eventually drain and complete.
	waitFor(t, 5*time.Second, "B completed", func() bool {
		return outbox.CompletedCount() >= 2
	})
}

// TestFanOut_RegisterSessionSenderWhileRunning verifies that
// RegisterSessionSender returns an error if called after Start.
func TestFanOut_RegisterSessionSenderWhileRunning(t *testing.T) {
	rt := newTestRuntime("bridge-regfail", nil, nil, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	cfg := goruntime.RouteConfig{
		ID:                 "r1",
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	err := rt.RegisterSessionSender(
		fastSessionConfig("late-session"),
		NewFakeSession(),
		NewFakeSender(),
	)
	if err == nil {
		t.Fatal("expected error registering session sender while running")
	}
}
