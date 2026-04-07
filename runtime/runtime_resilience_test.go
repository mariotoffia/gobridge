package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════
// Runtime Resilience Tests (F2–F7)
//
// Tests for lease release context (F2), drain-on-shutdown (F3),
// direct_hold fencing validation (F4), TOCTOU removal (F5),
// scoped stale token errors (F6), and session re-Start (F7).
// ═══════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// F2: Lease Release Uses Expired Context During Timeout Shutdown
// ---------------------------------------------------------------------------

// TestF2_StopReleasesLeaseWithValidContext validates that Runtime.Stop
// passes a non-expired context to SessionManager.Close even when the
// caller's stop context has already expired.
func TestF2_StopReleasesLeaseWithValidContext(t *testing.T) {
	trackLease := NewContextTrackingLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-f2"),
		goruntime.WithLeaseStore(trackLease),
		goruntime.WithOutboxStore(NewFakeOutboxStore()),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewSlowExitReceiver(500 * time.Millisecond)
	sender := NewFakeSender()
	session := NewFakeSession()

	sessCfg := fastSessionConfig("sess-f2")
	sessCfg.LeaseTTL = 2 * time.Second
	sessCfg.RenewInterval = 200 * time.Millisecond
	sessCfg.MaxRenewFails = 5

	cfg := goruntime.RouteConfig{
		ID:     "f2-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: "sess-f2"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, session, &sessCfg); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	// Wait for the lease to be acquired — session.Start() returns before
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
// F3: No Drain-on-Shutdown
// ---------------------------------------------------------------------------

// TestF3_DrainOnShutdown validates that the OutboxDrainer delivers
// pending records during its lifecycle. A fast poll interval ensures
// at least one drain cycle runs before cancellation.
func TestF3_DrainOnShutdown(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Strategy = domain.NewFixedPoll(10 * time.Millisecond)
	})

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-f3",
		RouteID:    "route-1",
		EnvelopeID: "env-f3",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-f3", Payload: []byte("shutdown-drain")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithCancel(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- drainer.Run(drainCtx) }()

	time.Sleep(100 * time.Millisecond)
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

// TestF3_DrainOnShutdown_NoLease validates that the final drain sweep
// is skipped when the drainer does not hold a lease.
func TestF3_DrainOnShutdown_NoLease(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	_, sender, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Strategy = domain.NewFixedPoll(10 * time.Second)
		cfg.TokenFn = func() (domain.LeaseToken, bool) {
			return domain.LeaseToken{}, false
		}
	})

	drainCtx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- drainer.Run(drainCtx) }()

	time.Sleep(50 * time.Millisecond)
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
// F4: No Fencing for direct_hold Mode
// ---------------------------------------------------------------------------

// TestF4_DirectHoldSharedConsumerRejected validates that the validator
// rejects direct_hold delivery with a shared consumer source.
func TestF4_DirectHoldSharedConsumerRejected(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("bridge-f4-reject"))
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "f4-reject",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{
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

// TestF4_DirectHoldAllowUnfenced validates that the AllowUnfenced policy
// flag bypasses the shared consumer fencing check for direct_hold.
func TestF4_DirectHoldAllowUnfenced(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("bridge-f4-allow"))
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "f4-allow",
		Policy: domain.RoutePolicy{
			DeliveryMode:  domain.DeliveryDirectHold,
			AllowUnfenced: true,
		},
		SourceCapabilities: []ports.Capability{
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
// F5: Unnecessary TOCTOU Pre-Check
// ---------------------------------------------------------------------------

// TestF5_DrainBatchSkipsTOCTOUCheck validates that drainBatch does not
// call leaseStore.Current() before Claim(). The Claim conditional write
// provides sufficient fencing.
func TestF5_DrainBatchSkipsTOCTOUCheck(t *testing.T) {
	countLease := NewCountingLeaseStore()
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	_, _ = countLease.inner.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		LeaseStore:   countLease,
		Sender:       sender,
		DLQ:          goruntime.NewDLQRouter(nil),
		RouteID:      "route-1",
		PartitionKey: domain.OutboxPartitionKey("sess-1", ""),
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy:       domain.RoutePolicy{}.WithDefaults(),
		Strategy:     domain.NewFixedPoll(50 * time.Millisecond),
		TokenFn:      func() (domain.LeaseToken, bool) { return token, true },
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	rec := domain.OutboxRecord{
		ID: "rec-f5", RouteID: "route-1", EnvelopeID: "env-f5",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-f5", Payload: []byte("data")},
		Status:   domain.OutboxPending,
	}
	_ = outbox.Persist(context.Background(), []domain.OutboxRecord{rec})

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
// F6: Stale Fencing Token Kills Entire Runtime
// ---------------------------------------------------------------------------

// TestF6_StaleFencingTokenDoesNotKillRuntime validates that a stale
// fencing token from one drainer does not mark the entire runtime
// unhealthy or cancel other routes.
func TestF6_StaleFencingTokenDoesNotKillRuntime(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-f6", outbox, lease, nil)

	receiverA := NewFakeReceiver()
	senderA := NewFakeSender()
	sessionA := NewFakeSession()
	sessCfgA := fastSessionConfig("sess-f6a")

	cfgA := goruntime.RouteConfig{
		ID:     "route-f6a",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: "sess-f6a"},
		},
	}
	_ = rt.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)

	receiverB := NewFakeReceiver()
	senderB := NewFakeSender()

	cfgB := goruntime.RouteConfig{
		ID:                 "route-f6b",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	_ = rt.AddRoute(cfgB, receiverB, senderB, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session A started", func() bool {
		return sessionA.IsStarted()
	})

	outbox.SetClaimErr(domain.ErrStaleFencingToken)

	time.Sleep(200 * time.Millisecond)

	if !rt.Healthy() {
		t.Error("runtime should remain healthy after scoped stale fencing token error")
	}
	if !rt.IsRunning() {
		t.Error("runtime should remain running after scoped stale fencing token error")
	}

	env := &domain.Envelope{ID: "f6-msg", Payload: []byte("test-f6")}
	del := NewFakeDelivery(env)
	_ = receiverB.Emit(ctx, del)

	waitFor(t, time.Second, "route B delivery acked", func() bool {
		return del.IsAcked()
	})
}

// TestF6_CriticalErrorStillKillsRuntime validates that non-fencing
// errors (e.g. fatal receiver failure) still mark the runtime unhealthy.
func TestF6_CriticalErrorStillKillsRuntime(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("bridge-f6-crit"))

	receiver := NewFakeReceiver()
	receiver.RunErr = errors.New("fatal receiver error")
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID:                 "crit-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())

	waitFor(t, 2*time.Second, "runtime unhealthy", func() bool {
		return !rt.Healthy()
	})

	_ = rt.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// F7: ConnectAfterLease Doesn't Re-Start() on Re-acquisition
// ---------------------------------------------------------------------------

// TestF7_ReacquiredLeaseRestartsDeadSession validates that after a
// lease gap where the broker disconnected the session, re-acquisition
// calls session.Start() again to re-establish the connection.
func TestF7_ReacquiredLeaseRestartsDeadSession(t *testing.T) {
	session := NewControllableSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:         "sess-f7",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          500 * time.Millisecond,
		RenewInterval:     50 * time.Millisecond,
		RenewJitter:       0,
		MaxRenewFails:     2,
		StepDownGrace:     50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Run(ctx) }()

	waitFor(t, 2*time.Second, "first start", func() bool {
		return session.GetStartCount() >= 1
	})

	session.SetConnected(false)

	leaseStore.SetRenewErr(domain.ErrVersionMismatch)

	waitFor(t, 3*time.Second, "lease lost", func() bool {
		_, has := mgr.Token()
		return !has
	})

	leaseStore.SetRenewErr(nil)

	waitFor(t, 5*time.Second, "session restarted", func() bool {
		return session.GetStartCount() >= 2
	})

	cancel()
	<-errCh
}

// TestF7_ReacquiredLeaseSkipsRestartIfHealthy validates that a healthy
// session is NOT restarted on lease re-acquisition.
func TestF7_ReacquiredLeaseSkipsRestartIfHealthy(t *testing.T) {
	session := NewControllableSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:         "sess-f7b",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          500 * time.Millisecond,
		RenewInterval:     50 * time.Millisecond,
		RenewJitter:       0,
		MaxRenewFails:     2,
		StepDownGrace:     50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = mgr.Run(ctx) }()

	waitFor(t, 2*time.Second, "first start", func() bool {
		return session.GetStartCount() >= 1
	})

	leaseStore.SetRenewErr(domain.ErrVersionMismatch)

	waitFor(t, 3*time.Second, "lease lost", func() bool {
		_, has := mgr.Token()
		return !has
	})

	leaseStore.SetRenewErr(nil)

	waitFor(t, 3*time.Second, "lease re-acquired", func() bool {
		_, has := mgr.Token()
		return has
	})

	time.Sleep(100 * time.Millisecond)

	if c := session.GetStartCount(); c != 1 {
		t.Fatalf("expected 1 start call (healthy session), got %d", c)
	}

	cancel()
}
