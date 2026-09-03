package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// ═══════════════════════════════════════════════════════════════════
// Runtime Resilience Tests
//
// Tests for lease release context, drain-on-shutdown,
// direct_hold fencing validation, TOCTOU removal,
// scoped stale token errors, and sess re-Start.
// ═══════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// Lease Release Uses Expired Context During Timeout Shutdown
// ---------------------------------------------------------------------------

// TestStopReleasesLeaseWithValidContext validates that Runtime.Stop
// passes a non-expired context to SessionManager.Close even when the
// caller's stop context has already expired.
func TestStopReleasesLeaseWithValidContext(t *testing.T) {
	trackLease := NewContextTrackingLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-f2"),
		goruntime.WithLeaseStore(trackLease),
		goruntime.WithOutboxStore(NewFakeOutboxStore()),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewSlowExitReceiver(500 * time.Millisecond)
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("sess-f2")
	sessCfg.LeaseTTL = 2 * time.Second
	sessCfg.RenewInterval = 200 * time.Millisecond
	sessCfg.MaxRenewFails = 5

	cfg := goruntime.RouteConfig{
		ID:     "f2-route",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "sess-f2"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// Wait for the lease to be acquired — sess.Start() returns before
	// runExclusive calls acquireLeaseWithRetry, so IsStarted() alone does
	// not guarantee hasLease is true when Stop is called.
	waitFor(t, 3*time.Second, "lease acquired", func() bool {
		info, err := trackLease.Current(context.Background(), "sess-f2")
		return err == nil && info.Version >= 1
	})

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = rt.Stop(cancelledCtx)

	if !trackLease.WasReleaseCalled() {
		t.Fatal("expected lease release to be called during Stop")
	}
	if !trackLease.IsReleaseCtxValid() {
		t.Fatal("expected lease release context to be valid (fresh context, not expired)")
	}
}

// ---------------------------------------------------------------------------
// No Drain-on-Shutdown
// ---------------------------------------------------------------------------

// TestDrainOnShutdown validates that the OutboxDrainer delivers
// pending records during its lifecycle. A fast poll interval ensures
// at least one drain cycle runs before cancellation.
func TestDrainOnShutdown(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Strategy = persistence.NewFixedPoll(10 * time.Millisecond)
	})

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-f3",
		RouteID:    "route-1",
		EnvelopeID: "env-f3",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-f3", Payload: []byte("shutdown-drain")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithCancel(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- drainer.Run(drainCtx) }()

	waitFor(t, 2*time.Second, "drainer sent 1", func() bool { return sender.SentCount() >= 1 })
	cancel()

	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run should exit after context cancellation")
	}

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if outbox.CompletedCount() != 1 {
		t.Fatalf("expected 1 completed, got %d", outbox.CompletedCount())
	}
}

// TestDrainOnShutdown_NoLease validates that the final drain sweep
// is skipped when the drainer does not hold a lease.
func TestDrainOnShutdown_NoLease(t *testing.T) {
	polled := make(chan struct{}, 256)
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	_, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Strategy = persistence.NewFixedPoll(10 * time.Millisecond)
		cfg.TokenFn = func() (persistence.LeaseToken, bool) {
			polled <- struct{}{}
			return persistence.LeaseToken{}, false
		}
	})
	drainCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- drainer.Run(drainCtx) }()

	waitFor(t, 2*time.Second, "drainer polled and skipped (no lease)", func() bool { return len(polled) >= 1 })
	cancel()

	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run should exit after context cancellation")
	}

	if sender.SentCount() != 0 {
		t.Fatal("should not drain when lease is not held")
	}
}

// ---------------------------------------------------------------------------
// No Fencing for direct_hold Mode
// ---------------------------------------------------------------------------

// TestDirectHoldSharedConsumerRejected validates that the validator
// rejects direct_hold delivery with a shared consumer source.
func TestDirectHoldSharedConsumerRejected(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("bridge-f4-reject"))
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "f4-reject",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
			ports.CapSharedConsumer,
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	err := rt.Start(context.Background())
	if err == nil {
		_ = rt.Stop(context.Background())
		t.Fatal("expected validation error for shared consumer with direct_hold")
	}
}

// TestDirectHoldAllowUnfenced validates that the AllowUnfenced policy
// flag bypasses the shared consumer fencing check for direct_hold.
func TestDirectHoldAllowUnfenced(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-f4-allow"),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "f4-allow",
		Policy: routing.RoutePolicy{
			DeliveryMode:  routing.DeliveryDirectHold,
			AllowUnfenced: true,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
			ports.CapSharedConsumer,
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	err := rt.Start(context.Background())
	if err != nil {
		t.Fatalf("AllowUnfenced should bypass shared consumer check: %v", err)
	}

	_ = rt.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// Unnecessary TOCTOU Pre-Check
// ---------------------------------------------------------------------------

// TestDrainBatchSkipsTOCTOUCheck validates that drainBatch does not
// call leaseStore.Current() before Claim(). The Claim conditional write
// provides sufficient fencing.
func TestDrainBatchSkipsTOCTOUCheck(t *testing.T) {
	countLease := NewCountingLeaseStore()
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	_, _ = countLease.inner.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()

	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   countLease,
		Sender:       sender,
		DLQ:          dlq.New(nil),
		RouteID:      "route-1",
		PartitionKey: persistence.OutboxPartitionKey("sess-1", ""),
		LeaseID:      "sess-1",
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		TokenFn:      func() (persistence.LeaseToken, bool) { return token, true },
	}
	drainer := outboxpkg.New(cfg)

	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-f5", RouteID: "route-1", EnvelopeID: "env-f5",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-f5", Payload: []byte("data")}),
		Status:   persistence.OutboxPending,
	})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if calls := countLease.GetCurrentCalls(); calls > 0 {
		t.Fatalf("expected 0 Current() calls (TOCTOU removed), got %d", calls)
	}
	if sender.SentCount() == 0 {
		t.Fatal("expected at least 1 send (Claim must have run)")
	}
}

// ---------------------------------------------------------------------------
// Stale Fencing Token Kills Entire Runtime
// ---------------------------------------------------------------------------

// TestStaleFencingTokenDoesNotKillRuntime validates that a stale
// fencing token from one drainer does not mark the entire runtime
// unhealthy or cancel other routes.
func TestStaleFencingTokenDoesNotKillRuntime(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-f6", outbox, lease, nil)

	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()
	sessCfgA := fastSessionConfig("sess-f6a")

	cfgA := goruntime.RouteConfig{
		ID:     "route-f6a",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "sess-f6a"},
		},
	}
	_ = rt.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)

	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()

	cfgB := goruntime.RouteConfig{
		ID:                 "route-f6b",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	_ = rt.AddRoute(cfgB, receiverB, senderB, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess A started", func() bool {
		return sessionA.IsStarted()
	})

	outbox.SetClaimErr(shared.ErrStaleFencingToken)

	time.Sleep(200 * time.Millisecond) // NEGATIVE: verify stale fencing token does not kill runtime

	if !rt.Healthy() {
		t.Error("runtime should remain healthy after scoped stale fencing token error")
	}
	if !rt.IsRunning() {
		t.Error("runtime should remain running after scoped stale fencing token error")
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "f6-msg", Payload: []byte("test-f6")})
	del := NewFakeDelivery(env)
	_ = receiverB.Emit(ctx, del)

	waitFor(t, time.Second, "route B delivery acked", func() bool {
		return del.IsAcked()
	})
}

// TestCriticalErrorStillKillsRuntime validates that non-fencing
// errors (e.g. fatal receiver failure) still mark the runtime unhealthy.
func TestCriticalErrorStillKillsRuntime(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-f6-crit"),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	receiver.RunErr = errors.New("fatal receiver error")
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID:                 "crit-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())

	waitFor(t, 2*time.Second, "runtime unhealthy", func() bool {
		return !rt.Healthy()
	})

	_ = rt.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// ConnectAfterLease Doesn't Re-Start() on Re-acquisition
// ---------------------------------------------------------------------------

// TestReacquiredLeaseRestartsDeadSession validates that after a
// lease gap where the broker disconnected the sess, re-acquisition
// calls sess.Start() again to re-establish the connection.
func TestReacquiredLeaseRestartsDeadSession(t *testing.T) {
	sess := NewControllableSession()
	leaseStore := NewFakeLeaseStore()

	cfg := session.Config{
		SessionID:         "sess-f7",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          500 * time.Millisecond,
		RenewInterval:     50 * time.Millisecond,
		RenewJitter:       0,
		MaxRenewFails:     2,
		StepDownGrace:     50 * time.Millisecond,
	}

	mgr := session.NewFromConfig(cfg, sess, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Run(ctx) }()

	waitFor(t, 2*time.Second, "first start", func() bool {
		return sess.GetStartCount() >= 1
	})

	sess.SetConnected(false)

	leaseStore.SetRenewErr(shared.ErrVersionMismatch)

	waitFor(t, 3*time.Second, "lease lost", func() bool {
		_, has := mgr.Token()
		return !has
	})

	leaseStore.SetRenewErr(nil)

	waitFor(t, 5*time.Second, "sess restarted", func() bool {
		return sess.GetStartCount() >= 2
	})

	cancel()
	<-errCh
}

// TestStepDownClosesSessionAndReacquireRestarts validates the
// cluster-safety fix on the connect-after-lease (deferred) path: losing the
// lease must Close the source session so a non-owner stops consuming/ACKing
// source messages during failover (no split-brain), and re-acquiring the
// lease must Start it again so the new owner resumes consuming.
//
// This replaces an earlier test that asserted a "healthy" session is NOT
// restarted on re-acquisition — that assertion encoded the very bug fixes
// (the receiver was never stopped on step-down, so it always looked healthy).
func TestStepDownClosesSessionAndReacquireRestarts(t *testing.T) {
	sess := NewControllableSession()
	leaseStore := NewFakeLeaseStore()

	cfg := session.Config{
		SessionID:         "sess-f7b",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          500 * time.Millisecond,
		RenewInterval:     50 * time.Millisecond,
		RenewJitter:       0,
		MaxRenewFails:     2,
		StepDownGrace:     50 * time.Millisecond,
	}

	mgr := session.NewFromConfig(cfg, sess, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = mgr.Run(ctx) }()

	waitFor(t, 2*time.Second, "first start", func() bool {
		return sess.GetStartCount() >= 1
	})

	leaseStore.SetRenewErr(shared.ErrVersionMismatch)

	// Step-down must Close the source session: a non-owner must stop
	// consuming source messages while the new owner takes over.
	waitFor(t, 3*time.Second, "source session closed on step-down", func() bool {
		return sess.GetCloseCount() >= 1
	})

	waitFor(t, 3*time.Second, "lease lost", func() bool {
		_, has := mgr.Token()
		return !has
	})

	leaseStore.SetRenewErr(nil)

	// Re-acquisition must restart the source session (receiver restored).
	waitFor(t, 5*time.Second, "source session restarted on re-acquire", func() bool {
		return sess.GetStartCount() >= 2
	})

	cancel()
}
