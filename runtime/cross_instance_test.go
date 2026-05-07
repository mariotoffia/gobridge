package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// TestCrossInstance_SQSConsumerAndMQTTOwnerAreDifferent verifies that one
// bridge instance can consume from SQS and persist to the shared outbox,
// while a different bridge instance holding the MQTT session lease drains
// the outbox and publishes. This is the core T11 cross-instance handoff.
func TestCrossInstance_SQSConsumerAndMQTTOwnerAreDifferent(t *testing.T) {
	// Shared stores visible to both instances.
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	const sessionID = "mqtt-exclusive-session"

	// --- Instance A: SQS ingress only, no MQTT session ---

	rtA := newTestRuntime("bridge-A", outbox, lease, dlq)

	receiverA := NewFakeReceiver()
	senderA := NewFakeSender() // won't be used for actual sending

	cfgA := goruntime.RouteConfig{
		ID: "route-sqs-mqtt",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "factory/a/orders/42"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}

	// Instance A has no MQTT session — it only runs the receiver and
	// outbox persist pipeline. Pass nil for session.
	if err := rtA.AddRoute(cfgA, receiverA, senderA, nil, nil); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}

	// --- Instance B: MQTT session owner, drains outbox ---

	rtB := newTestRuntime("bridge-B", outbox, lease, dlq)

	receiverB := NewFakeReceiver() // not actively used
	senderB := NewFakeSender()     // the MQTT sender
	sessionB := NewFakeSession()

	sessCfgB := fastSessionConfig(sessionID)

	cfgB := goruntime.RouteConfig{
		ID: "route-sqs-mqtt",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "factory/a/orders/42"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}

	if err := rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start both instances.
	if err := rtA.Start(ctx); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	defer func() { _ = rtA.Stop(context.Background()) }()

	if err := rtB.Start(ctx); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() { _ = rtB.Stop(context.Background()) }()

	// Wait for instance B to acquire the lease and start the session.
	waitFor(t, 3*time.Second, "session B started", func() bool {
		return sessionB.IsStarted()
	})

	// Instance A receives a message from SQS.
	env := &messaging.Envelope{
		ID:      "cross-msg-1",
		Payload: []byte("work-order"),
	}
	del := NewFakeDelivery(env)
	if err := receiverA.Emit(ctx, del); err != nil {
		t.Fatalf("Emit on A: %v", err)
	}

	// Source should be acked by instance A after outbox persist.
	waitFor(t, time.Second, "delivery acked by A", func() bool {
		return del.IsAcked()
	})

	// Instance A's sender should NOT have sent (it's not the session owner).
	if senderA.SentCount() != 0 {
		t.Fatalf("instance A sender should not send, got %d", senderA.SentCount())
	}

	// Instance B's drainer should pick up the record and send it.
	waitFor(t, 3*time.Second, "message sent by B", func() bool {
		return senderB.SentCount() >= 1
	})

	sent := senderB.GetSent()
	if sent[0].Subject != "" {
		t.Errorf("expected source-empty subject preserved on outbound, got %q", sent[0].Subject)
	}
	outboundB := senderB.GetOutbound()
	if len(outboundB) == 0 || outboundB[0].Address != "factory/a/orders/42" {
		t.Errorf("expected OutboundMessage.Address factory/a/orders/42, got %+v", outboundB)
	}
	if sent[0].ID != "cross-msg-1" {
		t.Errorf("expected envelope ID cross-msg-1, got %q", sent[0].ID)
	}
}

// TestCrossInstance_LeaseTransferDrainsRemaining verifies that when
// instance A holds the lease and crashes, instance B acquires the lease
// and drains the outbox records that A persisted but never sent.
func TestCrossInstance_LeaseTransferDrainsRemaining(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	const sessionID = "mqtt-failover-session"

	// --- Instance A: starts first, acquires lease ---
	ctxA, cancelA := context.WithCancel(context.Background())

	rtA := newTestRuntime("bridge-A-failover", outbox, lease, dlq)

	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 300 * time.Millisecond
	sessCfgA.RenewInterval = 60 * time.Millisecond

	cfgA := goruntime.RouteConfig{
		ID: "failover-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)
	_ = rtA.Start(ctxA)

	// Wait for A to get the lease.
	waitFor(t, 2*time.Second, "session A started", func() bool {
		return sessionA.IsStarted()
	})

	// A receives and persists messages to outbox.
	dels := emitMessages(t, ctxA, receiverA, "failover-msg", 3)

	// Wait for all to be acked.
	for i, d := range dels {
		waitFor(t, time.Second, "delivery acked "+string(rune('0'+i)), func() bool {
			return d.IsAcked()
		})
	}

	// A may have sent some of these via its drainer. Block A's sender
	// from sending more by making it fail, then crash A.
	senderA.SetSendErr(shared.NewBridgeError("SIMULATED_CRASH", shared.ErrorTransient, "crash"))

	// Crash instance A.
	cancelA()
	_ = rtA.Stop(context.Background())

	// --- Instance B: acquires lease after A crashes ---
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := newTestRuntime("bridge-B-failover", outbox, lease, dlq)

	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()
	sessionB := NewFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 300 * time.Millisecond
	sessCfgB.RenewInterval = 60 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "failover-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)
	_ = rtB.Start(ctxB)

	// Wait for B to acquire the lease (after A's lease expires).
	waitFor(t, 3*time.Second, "session B started", func() bool {
		return sessionB.IsStarted()
	})

	// B should drain any pending records from the outbox.
	// Count what A sent + what B sent should cover all 3 messages.
	waitFor(t, 3*time.Second, "all messages completed", func() bool {
		return outbox.CompletedCount() >= 3
	})

	_ = rtB.Stop(context.Background())

	// Verify all 3 messages were ultimately completed.
	if outbox.CompletedCount() < 3 {
		t.Fatalf("expected all 3 messages completed, got %d", outbox.CompletedCount())
	}
}

// TestCrossInstance_ConnectAfterLease verifies that with ConnectAfterLease
// set, the session is not started until the lease is acquired.
func TestCrossInstance_ConnectAfterLease(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-deferred", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	session := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-deferred")
	sessCfg.ConnectAfterLease = true

	// Pre-acquire the lease so this instance cannot get it immediately.
	_, _ = lease.Acquire(context.Background(), "mqtt-deferred", "other-owner", 500*time.Millisecond, nil)

	cfg := goruntime.RouteConfig{
		ID: "deferred-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-deferred"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(100 * time.Millisecond) // NEGATIVE: verify session not started before lease acquisition
	if session.IsStarted() {
		t.Fatal("session should not start before lease acquisition")
	}

	// Wait for the other owner's lease to expire, then our instance acquires.
	waitFor(t, 3*time.Second, "session started after lease acquired", func() bool {
		return session.IsStarted()
	})
}

// TestCrossInstance_MultipleMessages verifies a steady stream of messages
// flows through the shared outbox path from one instance to another.
func TestCrossInstance_MultipleMessages(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	const sessionID = "mqtt-stream"

	// Instance A: ingress only.
	rtA := newTestRuntime("bridge-stream-A", outbox, lease, nil)
	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()

	cfgA := goruntime.RouteConfig{
		ID: "stream-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}
	_ = rtA.AddRoute(cfgA, receiverA, senderA, nil, nil)

	// Instance B: egress owner.
	rtB := newTestRuntime("bridge-stream-B", outbox, lease, nil)
	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()
	sessionB := NewFakeSession()
	sessCfgB := fastSessionConfig(sessionID)

	cfgB := goruntime.RouteConfig{
		ID: "stream-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}
	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rtA.Start(ctx)
	_ = rtB.Start(ctx)
	defer func() {
		_ = rtA.Stop(context.Background())
		_ = rtB.Stop(context.Background())
	}()

	waitFor(t, 3*time.Second, "session B started", func() bool {
		return sessionB.IsStarted()
	})

	const msgCount = 10
	dels := emitMessages(t, ctx, receiverA, "stream", msgCount)

	for i, d := range dels {
		waitFor(t, time.Second, "ack "+string(rune('0'+i)), func() bool {
			return d.IsAcked()
		})
	}

	// All messages should be drained and sent by B.
	waitFor(t, 5*time.Second, "all messages sent by B", func() bool {
		return senderB.SentCount() >= msgCount
	})

	waitFor(t, 3*time.Second, "all completed", func() bool {
		return outbox.CompletedCount() >= msgCount
	})
}
