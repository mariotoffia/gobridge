package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSessionManager_StepDown_SkipsGraceWhenDrainIdle is the regression test for
// finding F9: stepDown must NOT wait out the full StepDownGrace when the
// destination outbox drainer reports idle (no in-flight records to settle) —
// waiting then only adds takeover latency because a new owner keys off the lease
// store, not this wait.
//
// The grace uses the REAL clock (context deadlines are not driven by the
// injected fake clock), so the test cannot advance it away. Instead it sets a
// deliberately huge StepDownGrace and asserts the lease is Released promptly:
// Release happens AFTER the grace inside stepDown, so if the idle early-exit
// regressed, Release would be blocked for the full 60s and the bounded wait
// below fails fast (well before it).
func TestSessionManager_StepDown_SkipsGraceWhenDrainIdle(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	// Succeed the first renew, fail the second: ErrVersionMismatch is a
	// definitive loss, so the second renew steps down immediately.
	store := newLeaseLossStore(1, renewCh)
	sess := newCountingSession()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:     "sess-f9",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		// Huge on purpose: if the idle early-exit regressed, stepDown would block
		// here for a full minute and the wait.Until below would fail fast.
		StepDownGrace: 60 * time.Second,
	}

	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))
	// Destination drainer has nothing in flight -> step-down may skip the grace.
	mgr.SetDrainIdleCheck(func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = mgr.Run(ctx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
		_ = mgr.Close(context.Background())
	}()

	// Owner starts the source session once.
	select {
	case <-sess.startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("source session was not started on lease acquisition")
	}

	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})
	// First renew succeeds.
	fake.Advance(renewInterval)
	select {
	case <-renewCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first renew did not fire after advance")
	}
	wait.Until(t, 2*time.Second, "renew timer reset", func() bool {
		return fake.TimerCount() >= 1
	})
	// Second renew fails (definitive) -> step-down.
	fake.Advance(renewInterval)

	// Close happens before the grace regardless; the discriminating assertion is
	// that Release (which follows the grace) lands promptly rather than after the
	// 60s grace.
	select {
	case <-sess.closedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("source session was NOT closed on step-down")
	}
	wait.Until(t, 3*time.Second, "lease released without waiting the full grace", func() bool {
		return store.releaseCount() >= 1
	})
}

// TestSessionManager_StepDown_WaitsGraceWhenDrainBusy is the complementary
// regression for finding F9: when the destination drainer reports BUSY (has
// claimable pending outbox work), stepDown MUST wait out the full StepDownGrace
// before releasing the lease so in-flight Send+Complete can settle in-process —
// the early-exit is idle-ONLY. A regression that skips the grace unconditionally
// would still pass the idle test above, so this one asserts the wait actually
// happens.
//
// The grace runs on the REAL clock (a detached context deadline, not the
// injected fake clock), so this measures wall-clock elapsed between the source
// Close (which stepDown performs BEFORE the grace) and the lease Release (which
// follows the grace). A modest 300ms grace keeps the test fast; the assertion
// uses a generous lower bound (>= 150ms) that a skipped grace (~0ms) cannot
// meet, while a real 300ms timer clears it comfortably even under -race load
// (timers fire late, never early). No time.Sleep is used — the wait is on the
// Release itself.
func TestSessionManager_StepDown_WaitsGraceWhenDrainBusy(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	store := newLeaseLossStore(1, renewCh)
	sess := newCountingSession()

	const (
		renewInterval = 500 * time.Millisecond
		grace         = 300 * time.Millisecond
	)
	cfg := Config{
		SessionID:     "sess-f9-busy",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		StepDownGrace: grace,
	}

	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))
	// Destination drainer still has claimable work -> step-down must NOT skip the
	// grace.
	mgr.SetDrainIdleCheck(func() bool { return false })

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = mgr.Run(ctx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
		_ = mgr.Close(context.Background())
	}()

	select {
	case <-sess.startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("source session was not started on lease acquisition")
	}

	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})
	fake.Advance(renewInterval) // first renew succeeds
	select {
	case <-renewCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first renew did not fire after advance")
	}
	wait.Until(t, 2*time.Second, "renew timer reset", func() bool {
		return fake.TimerCount() >= 1
	})
	fake.Advance(renewInterval) // second renew fails (definitive) -> step-down

	// Close is performed BEFORE the grace; anchor the elapsed measurement here.
	var closedAt time.Time
	select {
	case <-sess.closedCh:
		closedAt = time.Now()
	case <-time.After(3 * time.Second):
		t.Fatal("source session was NOT closed on step-down")
	}
	// Release follows the grace: it must land only AFTER the busy drainer's grace.
	wait.Until(t, 3*time.Second, "lease released after the step-down grace", func() bool {
		return store.releaseCount() >= 1
	})
	if elapsed := time.Since(closedAt); elapsed < grace/2 {
		t.Fatalf("busy drainer: step-down grace was skipped — Release came %v after Close, want >= %v", elapsed, grace/2)
	}
}
