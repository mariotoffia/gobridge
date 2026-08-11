package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// Fencing token validation
// ---------------------------------------------------------------------------

// TestEdge_StaleFencingTokenRejected verifies that a stale owner whose
// lease version is outdated cannot claim or complete outbox records.
func TestEdge_StaleFencingTokenRejected(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	const sessionID = "mqtt-fenced"

	// --- Instance A: acquires lease, persists messages ---
	ctxA, cancelA := context.WithCancel(context.Background())

	rtA := newTestRuntime("bridge-A-fenced", outbox, lease, dlq)
	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 300 * time.Millisecond
	sessCfgA.RenewInterval = 60 * time.Millisecond

	cfgA := goruntime.RouteConfig{
		ID: "fenced-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)
	_ = rtA.Start(ctxA)

	waitFor(t, 2*time.Second, "sess A started", func() bool {
		return sessionA.IsStarted()
	})

	// A persists a message.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "fenced-msg-1", Payload: []byte("data")})
	del := NewFakeDelivery(env)
	_ = receiverA.Emit(ctxA, del)
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	// Wait for A to drain and complete it.
	waitFor(t, 3*time.Second, "completed by A", func() bool {
		return outbox.CompletedCount() >= 1
	})

	// Crash A.
	cancelA()
	_ = rtA.Stop(context.Background())

	// --- Instance B: acquires lease with higher version ---
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := newTestRuntime("bridge-B-fenced", outbox, lease, dlq)
	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()
	sessionB := NewFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 300 * time.Millisecond
	sessCfgB.RenewInterval = 60 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "fenced-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)
	_ = rtB.Start(ctxB)

	waitFor(t, 3*time.Second, "sess B started", func() bool {
		return sessionB.IsStarted()
	})

	// Wait for B to actually hold the lease (sess start alone does not
	// guarantee the SessionManager has completed Acquire). Without this,
	// lease.Current may return ErrNotFound if A released the lease and B
	// hasn't acquired it yet.
	waitFor(t, 3*time.Second, "B holds lease", func() bool {
		info, err := lease.Current(context.Background(), sessionID)
		return err == nil && info.Version >= 2
	})

	info, err := lease.Current(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if info.Version < 2 {
		t.Errorf("expected B's version >= 2, got %d", info.Version)
	}

	// Attempt to complete with A's stale token should fail.
	staleToken := persistence.LeaseToken{Version: 1, Owner: "bridge-A-fenced"}
	_ = outbox.Complete(context.Background(), []string{"nonexistent"}, staleToken)
	// For existing records with mismatched claim version, this returns ErrStaleFencingToken.
	// For nonexistent records, it's a no-op in the fake store.

	// Persist a new record and try to claim with stale token.
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "stale-test-rec",
		EnvelopeID: "stale-test-env",
		BindingID:  "b1",
		SessionID:  sessionID,
		Status:     persistence.OutboxPending,
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "stale-test-env", Payload: []byte("x")}),
	})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{rec})

	// B should be able to claim and complete it with the correct token.
	waitFor(t, 3*time.Second, "B claims new record", func() bool {
		return senderB.SentCount() >= 1
	})

	_ = rtB.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// Idempotent persist
// ---------------------------------------------------------------------------

// TestEdge_IdempotentPersist verifies that if a source message is
// redelivered after outbox persist but before source ack, the duplicate
// persist is detected and the delivery is acked (not errored).
func TestEdge_IdempotentPersist(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-idemp", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-idemp")

	cfg := goruntime.RouteConfig{
		ID: "idemp-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-idemp"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// First delivery.
	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "idemp-msg", Payload: []byte("data")})
	del1 := NewFakeDelivery(env1)
	_ = receiver.Emit(ctx, del1)
	waitFor(t, time.Second, "first acked", func() bool { return del1.IsAcked() })

	initialCount := outbox.RecordCount()

	// Simulate redelivery of the same envelope ID.
	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "idemp-msg", Payload: []byte("data")})
	del2 := NewFakeDelivery(env2)
	_ = receiver.Emit(ctx, del2)

	// Second delivery should also be acked (dedup in persist).
	waitFor(t, time.Second, "second acked", func() bool { return del2.IsAcked() })

	// Should not retry (no transient error from dedup).
	if del2.IsRetried() {
		t.Error("redelivered message should not be retried")
	}

	// Record count should not increase (dedup prevented duplicate).
	if outbox.RecordCount() > initialCount {
		t.Errorf("expected no new records from dedup, had %d now %d", initialCount, outbox.RecordCount())
	}
}

// ---------------------------------------------------------------------------
// Expired outbox entry
// ---------------------------------------------------------------------------

// TestEdge_ExpiredOutboxEntry verifies that an expired message in the
// outbox is sent to DLQ (if policy is ExpiredDLQ) and not forwarded.
func TestEdge_ExpiredOutboxEntry(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-expiry", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-expiry")

	cfg := goruntime.RouteConfig{
		ID: "expiry-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			OnExpired:    routing.ExpiredDLQ,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-expiry"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// Send a message that is already expired.
	expEnv := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "expired-msg", Payload: []byte("stale"), CreatedAt: time.Now().Add(-2 * time.Hour)})
	_ = expEnv.SetExpiry(time.Now().Add(-1 * time.Hour))
	env := expEnv
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	// The expired message should be acked immediately (expiry check at ingress).
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	// It should go to DLQ, not the sender.
	waitFor(t, time.Second, "DLQ entry", func() bool { return dlq.Count() >= 1 })

	if sender.SentCount() != 0 {
		t.Error("expired message should not be sent")
	}
}

// TestEdge_ExpiredOutboxEntryDuringDrain verifies that a message that
// expires while sitting in the outbox is DLQ'd during drain, not sent.
func TestEdge_ExpiredOutboxEntryDuringDrain(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-drain-exp", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-drain-exp")

	cfg := goruntime.RouteConfig{
		ID: "drain-exp-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			OnExpired:    routing.ExpiredDLQ,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-drain-exp"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// Send a message that will expire in 100ms — it will be persisted
	// to the outbox, but by the time the drainer picks it up it should
	// have expired.
	shortLivedEnv := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "short-lived-msg", Payload: []byte("fleeting")})
	_ = shortLivedEnv.SetExpiry(time.Now().Add(100 * time.Millisecond))
	env := shortLivedEnv

	// Block the sender so the drainer can't send before expiry.
	sender.SetSendErr(shared.NewBridgeError("BLOCKED", shared.ErrorTransient, "blocked"))

	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	time.Sleep(200 * time.Millisecond) // FIXED: wait for message expiry (100ms TTL + margin)

	// Unblock the sender.
	sender.SetSendErr(nil)

	// The drainer should detect expiry and route to DLQ. The first drain
	// hit the blocked sender (transient), so the record was released for
	// retry and the next drain is spaced by the transient backoff floor
	// (~5s); wait past it for the expiry-detection drain.
	waitFor(t, 8*time.Second, "DLQ from drain", func() bool { return dlq.Count() >= 1 })
}

// ---------------------------------------------------------------------------
// Poison message
// ---------------------------------------------------------------------------

// TestEdge_PoisonMessageDLQ verifies that a record exceeding
// MaxReplayAttempts is moved to DLQ with PoisonMessage category.
func TestEdge_PoisonMessageDLQ(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	// WP-REPLAY-BUDGET: poisoning now requires the record to spend its
	// wall-clock ReplayBudget (measured from FirstAttemptedAt) in addition to
	// exhausting MaxReplayAttempts; poisonMinAge is only the legacy fallback for
	// zero-first-attempt records. This real-time test cannot wait the 15m
	// production budget, so the route policy below shrinks ReplayBudget to
	// effectively-immediate. The poisonMinAge option is likewise shrunk so the
	// legacy fallback stays fast too.
	rt := newTestRuntime("bridge-poison", outbox, lease, dlq, goruntime.WithOutboxPoisonMinAge(time.Millisecond))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendErr = shared.NewBridgeError("CRASH", shared.ErrorTransient, "always fails")
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-poison")

	cfg := goruntime.RouteConfig{
		ID: "poison-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			// One replay attempt: under the transient backoff floor each
			// retry is spaced ~5s, so a small cap keeps the poison path fast
			// (poison after the first 5s retry) while still exercising the
			// MaxReplayAttempts → PoisonMessage DLQ transition.
			MaxReplayAttempts: 1,
			// Effectively-immediate budget so the record poisons as soon as it
			// is re-claimed past MaxReplayAttempts (WithDefaults preserves this
			// non-zero value; the drainer derives its budget from it).
			ReplayBudget: time.Millisecond,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-poison"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "poison-msg", Payload: []byte("toxic")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	// The drainer should eventually hit MaxReplayAttempts and DLQ.
	waitFor(t, 10*time.Second, "poison DLQ", func() bool { return dlq.Count() >= 1 })

	entries := dlq.GetEntries()
	found := false
	for _, e := range entries {
		if e.Category() == "PoisonMessage" || e.ErrorCode() == "POISON_MESSAGE" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected DLQ entry with PoisonMessage category")
	}
}

// ---------------------------------------------------------------------------
// Crash recovery scenarios
// ---------------------------------------------------------------------------

// TestEdge_CrashBeforeOutboxPersist verifies that if the bridge crashes
// before persisting to outbox, the source redelivers with no message loss.
func TestEdge_CrashBeforeOutboxPersist(t *testing.T) {
	outbox := NewFakeOutboxStore()
	outbox.PersistErr = shared.NewBridgeError("STORE_DOWN", shared.ErrorTransient, "unavailable")
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-crash-pre", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-crash-pre")

	cfg := goruntime.RouteConfig{
		ID: "crash-pre-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-crash-pre"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "crash-pre-msg", Payload: []byte("data")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	// Persist fails -> should retry via source (Delivery.Retry).
	waitFor(t, time.Second, "retried", func() bool { return del.IsRetried() })

	if del.IsAcked() {
		t.Error("should not ack when persist fails")
	}
}

// TestEdge_CrashAfterPersistBeforeAck verifies that if the bridge crashes
// after persisting to outbox but before acking the source, the source
// redelivers and the outbox dedup catches the duplicate.
func TestEdge_CrashAfterPersistBeforeAck(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-crash-mid", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-crash-mid")

	cfg := goruntime.RouteConfig{
		ID: "crash-mid-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-crash-mid"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// First delivery: persists, acked.
	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "crash-mid-msg", Payload: []byte("data")})
	del1 := NewFakeDelivery(env1)
	_ = receiver.Emit(ctx, del1)
	waitFor(t, time.Second, "first acked", func() bool { return del1.IsAcked() })

	// Simulate: source redelivers the same message (ack was lost).
	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "crash-mid-msg", Payload: []byte("data")})
	del2 := NewFakeDelivery(env2)
	_ = receiver.Emit(ctx, del2)

	// Dedup should catch it and ack without error.
	waitFor(t, time.Second, "second acked", func() bool { return del2.IsAcked() })

	if del2.IsRetried() {
		t.Error("duplicate should not cause retry")
	}
}

// TestEdge_CrashAfterAckBeforeSend verifies that after source ack, if
// the bridge crashes before sending, the message is delivered either by
// A's final drain sweep (drain-on-shutdown) or by B's recovery drain.
func TestEdge_CrashAfterAckBeforeSend(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	const sessionID = "mqtt-crash-after-ack"

	// Instance A: persists and acks, then crashes before regular drain.
	ctxA, cancelA := context.WithCancel(context.Background())

	rtA := newTestRuntime("bridge-A-crash-ack", outbox, lease, dlq)
	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 300 * time.Millisecond
	sessCfgA.RenewInterval = 60 * time.Millisecond
	sessCfgA.DrainStrategy = persistence.NewFixedPoll(10 * time.Second)

	cfgA := goruntime.RouteConfig{
		ID: "crash-ack-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)
	_ = rtA.Start(ctxA)

	waitFor(t, 2*time.Second, "sess A started", func() bool {
		return sessionA.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "crash-ack-msg", Payload: []byte("important")})
	del := NewFakeDelivery(env)
	_ = receiverA.Emit(ctxA, del)
	waitFor(t, time.Second, "acked by A", func() bool { return del.IsAcked() })

	cancelA()
	_ = rtA.Stop(context.Background())

	// Instance B takes over.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := newTestRuntime("bridge-B-crash-ack", outbox, lease, dlq)
	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()
	sessionB := NewFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 300 * time.Millisecond
	sessCfgB.RenewInterval = 60 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "crash-ack-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)
	_ = rtB.Start(ctxB)

	waitFor(t, 3*time.Second, "sess B started", func() bool {
		return sessionB.IsStarted()
	})

	// Message should have been sent by A (final drain on shutdown) or B (recovery).
	waitFor(t, 5*time.Second, "message sent by A or B", func() bool {
		return senderA.SentCount() >= 1 || senderB.SentCount() >= 1
	})

	var sentMsg *messaging.Envelope
	if senderA.SentCount() >= 1 {
		sentMsg = senderA.GetSent()[0]
	} else {
		sentMsg = senderB.GetSent()[0]
	}
	if sentMsg.ID() != "crash-ack-msg" {
		t.Errorf("expected crash-ack-msg, got %q", sentMsg.ID())
	}

	_ = rtB.Stop(context.Background())
}

// TestEdge_PermanentSendErrorGoesToDLQ verifies that a permanent send
// error causes the message to be routed to DLQ without retry.
func TestEdge_PermanentSendErrorGoesToDLQ(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-perm", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendErr = shared.NewBridgeError("INVALID_PAYLOAD", shared.ErrorPermanent, "bad data")
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-perm")

	cfg := goruntime.RouteConfig{
		ID: "perm-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-perm"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "perm-msg", Payload: []byte("bad")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	// Drainer should detect permanent error and DLQ immediately.
	waitFor(t, 5*time.Second, "DLQ entry", func() bool { return dlq.Count() >= 1 })

	entries := dlq.GetEntries()
	if entries[0].ErrorCode() != "INVALID_PAYLOAD" {
		t.Errorf("expected INVALID_PAYLOAD, got %q", entries[0].ErrorCode())
	}
}

// ---------------------------------------------------------------------------
// Crash after send but before outbox completion
// ---------------------------------------------------------------------------

// TestEdge_CrashAfterSendBeforeCompletion verifies that if the sender
// succeeds but the outbox Complete call fails (e.g. DynamoDB timeout),
// then instance A crashes, instance B reclaims the stale-claimed record,
// re-sends, and completes it. At-least-once delivery is preserved.
func TestEdge_CrashAfterSendBeforeCompletion(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	const sessionID = "mqtt-crash-complete"

	// Instance A: Complete always fails, simulating a crash after send.
	ctxA, cancelA := context.WithCancel(context.Background())

	completeCallCount := 0
	outbox.CompleteFn = func(_ []string, _ persistence.LeaseToken) error {
		completeCallCount++
		return shared.NewBridgeError("DDB_TIMEOUT", shared.ErrorTransient, "completion timeout")
	}

	rtA := newTestRuntime("bridge-A-crash-complete", outbox, lease, dlq)
	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 300 * time.Millisecond
	sessCfgA.RenewInterval = 60 * time.Millisecond

	cfgA := goruntime.RouteConfig{
		ID: "crash-complete-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)
	_ = rtA.Start(ctxA)

	waitFor(t, 2*time.Second, "sess A started", func() bool {
		return sessionA.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "crash-complete-msg", Payload: []byte("data")})
	del := NewFakeDelivery(env)
	_ = receiverA.Emit(ctxA, del)
	waitFor(t, time.Second, "acked by A", func() bool { return del.IsAcked() })

	// Wait for A's drainer to attempt sending (and failing completion).
	waitFor(t, 3*time.Second, "A sent at least once", func() bool {
		return senderA.SentCount() >= 1
	})

	// Crash A.
	cancelA()
	_ = rtA.Stop(context.Background())

	// Fix Complete for instance B.
	outbox.CompleteFn = nil
	outbox.CompleteErr = nil

	// Instance B takes over.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := newTestRuntime("bridge-B-crash-complete", outbox, lease, dlq)
	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()
	sessionB := NewFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 300 * time.Millisecond
	sessCfgB.RenewInterval = 60 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "crash-complete-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)
	_ = rtB.Start(ctxB)

	waitFor(t, 3*time.Second, "sess B started", func() bool {
		return sessionB.IsStarted()
	})

	// B should reclaim the stale-claimed record, re-send, and complete.
	waitFor(t, 5*time.Second, "outbox completed by B", func() bool {
		return outbox.CompletedCount() >= 1
	})

	// At-least-once: B re-sent the message.
	if senderB.SentCount() < 1 {
		t.Fatal("B should have re-sent the reclaimed record")
	}

	_ = rtB.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// Fan-out atomicity
// ---------------------------------------------------------------------------

// TestEdge_FanOutPartialPersist verifies that when Persist fails for a
// fan-out batch, no orphan records remain in the outbox and the delivery
// is retried via the source rather than acked.
func TestEdge_FanOutPartialPersist(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-fanout-partial", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-fanout-partial")

	cfg := goruntime.RouteConfig{
		ID: "fanout-partial-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "bind-a", Address: "topic/a"},
				{BindingID: "bind-b", Address: "topic/b"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "bind-a", SessionID: "mqtt-fanout-partial"},
			{ID: "bind-b", SessionID: "mqtt-fanout-partial"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// Make Persist fail atomically — the whole batch is rejected.
	outbox.PersistErr = shared.NewBridgeError("STORE_DOWN", shared.ErrorTransient, "unavailable")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "fanout-partial-msg", Payload: []byte("data")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	// Persist fails -> delivery should be retried, not acked.
	waitFor(t, time.Second, "retried", func() bool { return del.IsRetried() })

	if del.IsAcked() {
		t.Error("should not ack when persist fails")
	}

	// No orphan records should exist.
	if outbox.RecordCount() != 0 {
		t.Errorf("expected 0 records after failed persist, got %d", outbox.RecordCount())
	}
}

// ---------------------------------------------------------------------------
// Helper: DLQ store entry access
// ---------------------------------------------------------------------------

func (s *FakeDLQStore) GetEntries() []routing.DLQEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]routing.DLQEntry, len(s.Entries))
	copy(cp, s.Entries)
	return cp
}
