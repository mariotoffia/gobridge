package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Verifies one source message fanning out to multiple sessions persists per-plan outbox rows and each session drainer sends the correct address via RegisterSessionSender.
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
		Plans: []routing.DispatchPlan{
			{BindingID: "bind-factory-a", Address: "factory/a/orders/42"},
			{BindingID: "bind-factory-b", Address: "factory/b/orders/42"},
		},
	}

	cfg := goruntime.RouteConfig{
		ID: "fanout-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: resolver,
		Bindings: []routing.DestinationBinding{
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
	env := &messaging.Envelope{
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
	if sentA[0].Subject != "" {
		t.Errorf("sender A: expected logical Subject preserved (empty), got %q", sentA[0].Subject)
	}
	outA := senderA.GetOutbound()
	if len(outA) == 0 || outA[0].Address != "factory/a/orders/42" {
		t.Errorf("sender A: expected OutboundMessage.Address factory/a/orders/42, got %+v", outA)
	}

	sentB := senderB.GetSent()
	if sentB[0].Subject != "" {
		t.Errorf("sender B: expected logical Subject preserved (empty), got %q", sentB[0].Subject)
	}
	outB := senderB.GetOutbound()
	if len(outB) == 0 || outB[0].Address != "factory/b/orders/42" {
		t.Errorf("sender B: expected OutboundMessage.Address factory/b/orders/42, got %+v", outB)
	}

	// Both records should be completed.
	waitFor(t, 3*time.Second, "both completed", func() bool {
		return outbox.CompletedCount() >= 2
	})
}

// Verifies partial fan-out: one failing session leaves its outbox record incomplete until the sender recovers, while the other completes first.
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
	senderB.SendErr = shared.NewBridgeError("TARGET_DOWN", shared.ErrorTransient, "offline")
	sessionB := NewFakeSession()
	sessCfgB := fastSessionConfig("mqtt-partial-b")

	_ = rt.RegisterSessionSender(sessCfgB, sessionB, senderB)

	cfg := goruntime.RouteConfig{
		ID: "partial-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "ba", Address: "topic/a"},
				{BindingID: "bb", Address: "topic/b"},
			},
		},
		Bindings: []routing.DestinationBinding{
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

	env := &messaging.Envelope{ID: "partial-msg-1", Payload: []byte("data")}
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

// Verifies RegisterSessionSender returns an error when invoked after the runtime has started.
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
