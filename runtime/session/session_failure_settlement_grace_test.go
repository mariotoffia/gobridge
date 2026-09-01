package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Step-down and session-failure recovery both surrender a held lease while
// destination work this owner already accepted may still be settling. Step-down
// waits the bounded StepDownGrace before Release for exactly that reason: the
// standby that acquires next advances the fence, and a send still in flight
// here would land behind it as an accepted duplicate.
//
// The session-failure path closed the source and released immediately, so it
// widened the same duplicate window the grace exists to close. Closing the
// source stops INGRESS; it does not settle the destination sends already
// accepted from it. Both paths now share one bounded grace, and both skip it on
// the same evidence — a destination drainer that reports idle.

// TestSessionManager_SessionFailure_WaitsSettlementGraceBeforeRelease pins the
// grace on the session-failure path.
//
// The grace runs on the REAL clock (a detached context deadline, not the
// injected fake), so the assertion is the same shape the step-down grace test
// uses: a busy drainer plus a grace far larger than the test's patience means a
// prompt Release can only happen if the grace was skipped.
//
// Counterfactual (the pre-fix hand-off): afterRenewLoopExit called
// releaseOwnedLeaseBestEffort straight after the bounded close, so Release
// landed within milliseconds and this test's "no release yet" window fails.
func TestSessionManager_SessionFailure_WaitsSettlementGraceBeforeRelease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil) // always grant; never fail a renew
	sess := newSingleUseReconnectFailSession()

	mgr := NewWithMetrics(Config{
		SessionID:         "sess-failure-grace",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          30 * time.Second,
		RenewInterval:     30 * time.Second,
		RenewJitter:       0,
		RenewCallTimeout:  100 * time.Millisecond,
		MaxRenewFails:     3,
		// Long enough that a skipped grace and an honoured one are not the same
		// observation, short enough that the terminating wait below still ends.
		StepDownGrace: time.Second,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	// Consulting the drainer is the first thing the grace does, so this channel
	// marks the instant the wait begins — the deterministic point at which the
	// lease must still be held.
	graceEntered := make(chan struct{}, 1)
	mgr.SetDrainIdleCheck(func() bool {
		select {
		case graceEntered <- struct{}{}:
		default:
		}
		return false // destination drainer still has claimable work
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	wait.RequireReceive(t, graceEntered, 3*time.Second)
	require.GreaterOrEqual(t, sess.closeCalls.Load(), int32(1),
		"the source is fenced BEFORE the grace: closing stops ingress, the grace settles egress")
	require.Zero(t, store.releaseCount(),
		"the lease must still be held while accepted destination work settles")

	require.Error(t, wait.RequireReceive(t, done, 5*time.Second))
	require.Positive(t, store.releaseCount(),
		"the grace is bounded: the lease is released once it elapses")
}

// TestSessionManager_SessionFailure_SkipsSettlementGraceWhenDrainIdle is the
// complementary control. An idle destination drainer proves there is nothing
// left to settle, so waiting only delays the standby's takeover — the same
// early exit step-down already makes.
func TestSessionManager_SessionFailure_SkipsSettlementGraceWhenDrainIdle(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil)
	sess := newSingleUseReconnectFailSession()

	mgr := NewWithMetrics(Config{
		SessionID:         "sess-failure-grace-idle",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          30 * time.Second,
		RenewInterval:     30 * time.Second,
		RenewJitter:       0,
		RenewCallTimeout:  100 * time.Millisecond,
		MaxRenewFails:     3,
		// Huge on purpose: if the idle early-exit is missing, the release below
		// cannot land inside the wait.
		StepDownGrace: 60 * time.Second,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))
	mgr.SetDrainIdleCheck(func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	require.Error(t, wait.RequireReceive(t, done, 5*time.Second))
	wait.Until(t, 3*time.Second, "lease released without waiting the full grace", func() bool {
		return store.releaseCount() >= 1
	})
}
