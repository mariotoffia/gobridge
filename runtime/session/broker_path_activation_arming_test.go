package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// nonConvergedSince reads the broker-health outage clock under the manager's
// own lock, so a test can barrier on the event having been handled instead of
// racing the renew loop.
func (m *Manager) nonConvergedSince() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notConvergedSince
}

// A completed post-acquire activation IS the proof that this owner reached the
// broker: Start and Reconcile both returned. The broker-health outage clock
// must arm from that state, not from a delivered SessionConnected event —
// the transport's event channel drops its oldest entry under a storm, so an
// owner that genuinely converged can otherwise never arm and will hold the
// lease through an unbounded node-local broker outage.
func TestSessionManager_BrokerPathClockArmsFromActivation_NotEventDelivery(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &transientRenewStore{}
	sess := newCountingSession()

	const (
		leaseTTL             = 60 * time.Second
		renewInterval        = 5 * time.Second
		brokerHealthStepDown = 30 * time.Second
	)
	cfg := Config{
		SessionID:                    "sess-broker-path",
		Exclusive:                    true,
		ConnectAfterLease:            true,
		LeaseTTL:                     leaseTTL,
		RenewInterval:                renewInterval,
		RenewJitter:                  0,
		MaxRenewFails:                1,
		StepDownGrace:                20 * time.Millisecond,
		PostAcquireActivationTimeout: 10 * time.Second,
		BrokerHealthStepDown:         brokerHealthStepDown,
	}
	rec := &ports.RecordingExporter{}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, rec, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
	t.Cleanup(func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) })

	// Activation: the deferred connect and the initial reconcile both succeed —
	// this owner is converged on its broker path. No SessionConnected event is
	// ever delivered.
	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	// The broker path drops.
	select {
	case sess.events <- ports.SessionEvent{Type: ports.SessionDisconnected}:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop did not consume the disconnect event")
	}
	wait.Until(t, 2*time.Second, "broker-health outage clock armed by the disconnect",
		func() bool { return !mgr.nonConvergedSince().IsZero() })

	// Past the threshold, the next renew tick must step down so a healthy
	// standby can take over: source closed, lease released, metric emitted.
	fake.Advance(brokerHealthStepDown + renewInterval)
	wait.RequireReceive(t, sess.closedCh, 3*time.Second)
	wait.Until(t, 3*time.Second, "lease released on broker-path step-down",
		func() bool { return store.releaseCount() >= 1 })
	wait.Until(t, 2*time.Second, "BrokerHealthStepDown counter emitted",
		func() bool { return len(rec.FindEntries(shared.MetricBrokerHealthStepDown)) >= 1 })
}

// A new lease term has not converged on its broker path yet, so nothing about
// the PREVIOUS term's outage may decide anything in it. The outage clock is
// per-term state; carrying an armed timestamp into the next term makes the
// first renew tick — which fires while that term is still connecting and
// reconciling — step down at once on evidence about a session that is gone,
// and the two loops then trade the lease forever without either serving.
//
// The term here ends on a DEFINITIVE lease loss taken while the outage clock
// was already armed but before its threshold — a broker blip that overlaps a
// fencing loss. (A term ended BY the broker-path threshold does not re-acquire
// at all; see the escalation test below.)
func TestSessionManager_BrokerPathClockDoesNotCarryIntoTheNextTerm(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &transientRenewStore{}
	sess := newCountingSession()

	const (
		leaseTTL             = 10 * time.Minute
		renewInterval        = 5 * time.Second
		brokerHealthStepDown = 30 * time.Second
	)
	cfg := Config{
		SessionID:     "sess-term-reset",
		Exclusive:     true,
		LeaseTTL:      leaseTTL,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		StepDownGrace: 20 * time.Millisecond,
		// Generous, so the ONLY thing that can close the source in the parked
		// second term is a broker-health step-down — never the activation
		// deadline the test drives the clock past.
		PostAcquireActivationTimeout: 5 * time.Minute,
		BrokerHealthStepDown:         brokerHealthStepDown,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
	t.Cleanup(func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) })

	// First term: activate, lose the broker path, cross the threshold.
	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	// Park the NEXT term inside activation, so its renew loop is running while
	// it is still connecting — the window a stale timestamp fires in.
	gate := sess.gateReconcile()
	t.Cleanup(func() { close(gate) })

	select {
	case sess.events <- ports.SessionEvent{Type: ports.SessionDisconnected}:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop did not consume the disconnect event")
	}
	wait.Until(t, 2*time.Second, "broker-health outage clock armed", func() bool {
		return !mgr.nonConvergedSince().IsZero()
	})

	// End the term on a fencing loss, WELL BEFORE the broker-path threshold, so
	// the caller re-acquires with the outage clock still armed.
	store.failDefinitiveOnce.Store(true)
	fake.Advance(renewInterval)
	wait.RequireReceive(t, sess.closedCh, 3*time.Second)

	// Second term: re-acquired, reconnected, still inside its gated reconcile.
	wait.RequireReceive(t, sess.startedCh, 3*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 3*time.Second)
	require.True(t, mgr.nonConvergedSince().IsZero(),
		"a new lease term must start with a clear broker-health outage clock, not the previous term's")

	// Drive several renew ticks while the term is still activating. None may
	// step down: this term has never been converged, so it has no outage.
	closes := sess.closeCount()
	for range 3 {
		fake.Advance(renewInterval)
	}
	require.Never(t, func() bool { return sess.closeCount() > closes }, 300*time.Millisecond, 20*time.Millisecond,
		"a term still in post-acquire activation stepped down on the previous term's outage clock")
}

// An owner that steps down on its broker path has just PROVED it cannot serve
// this session. Falling back into the acquire loop lets it re-seize the
// partition microseconds after its own Release commits — it wins that race
// against a standby that is asleep for up to a full acquire poll — and the
// healthy standby then waits out a whole failed activation before it gets
// another chance. The step-down must be terminal for this process instead, so
// the hand-off it exists to perform actually happens.
func TestSessionManager_BrokerPathStepDown_DoesNotReSeizeTheLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &transientRenewStore{}
	sess := newCountingSession()

	const (
		renewInterval        = 5 * time.Second
		brokerHealthStepDown = 30 * time.Second
	)
	cfg := Config{
		SessionID:                    "sess-no-reseize",
		Exclusive:                    true,
		ConnectAfterLease:            true,
		LeaseTTL:                     10 * time.Minute,
		RenewInterval:                renewInterval,
		RenewJitter:                  0,
		MaxRenewFails:                1,
		StepDownGrace:                20 * time.Millisecond,
		PostAcquireActivationTimeout: 5 * time.Minute,
		BrokerHealthStepDown:         brokerHealthStepDown,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = mgr.Close(context.Background()) })

	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	select {
	case sess.events <- ports.SessionEvent{Type: ports.SessionDisconnected}:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop did not consume the disconnect event")
	}
	wait.Until(t, 2*time.Second, "broker-health outage clock armed", func() bool {
		return !mgr.nonConvergedSince().IsZero()
	})
	fake.Advance(brokerHealthStepDown + renewInterval)

	err := wait.RequireReceive(t, runErr, 5*time.Second)
	require.ErrorIs(t, err, ErrSessionUnrecoverable,
		"a broker-path step-down must escalate so the orchestrator restarts this process, not re-acquire in place")
	require.GreaterOrEqual(t, store.releaseCount(), int32(1), "the lease must be released before escalating")
	acquires := store.acquireCount()
	require.Equal(t, int32(1), acquires,
		"the stepping-down owner must not race the standby for the lease it just released; acquires=%d", acquires)
}

// Once activation completes, this owner is EXPECTED to be serving — so a broker
// path that is down from that instant is an outage, whether or not the owner
// ever reached the broker at all.
//
// The case that makes this matter is a lease-bearing session with no
// subscriptions — an exclusive EGRESS session: its Reconcile issues no broker
// call and returns nil against a disconnected transport, so activation
// "succeeds" having proved nothing. Waiting for a convergence that never
// happens would leave this owner holding the partition forever with no standby
// able to take it, which is the whole failure this threshold exists to bound.
func TestSessionManager_BrokerPathClockArmsWhenActivationCompletesDisconnected(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &transientRenewStore{}
	sess := newCountingSession()
	gate := sess.gateReconcile()

	const (
		renewInterval        = 5 * time.Second
		brokerHealthStepDown = 30 * time.Second
	)
	cfg := Config{
		SessionID:                    "sess-vacuous",
		Exclusive:                    true,
		LeaseTTL:                     10 * time.Minute,
		RenewInterval:                renewInterval,
		RenewJitter:                  0,
		MaxRenewFails:                1,
		StepDownGrace:                20 * time.Millisecond,
		PostAcquireActivationTimeout: 5 * time.Minute,
		BrokerHealthStepDown:         brokerHealthStepDown,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
	t.Cleanup(func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) })

	// The broker path drops while activation is parked in its (subscription-free,
	// therefore call-free) Reconcile, so activation completes without this owner
	// ever having reached the broker.
	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	sess.disconnect()
	close(gate)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	// Two sends: the second is only RECEIVED once the first has been fully
	// handled, which is the barrier this assertion needs.
	for range 2 {
		select {
		case sess.events <- ports.SessionEvent{Type: ports.SessionDisconnected}:
		case <-time.After(2 * time.Second):
			t.Fatal("renew loop did not consume the disconnect event")
		}
	}
	require.False(t, mgr.nonConvergedSince().IsZero(),
		"an activation that completed against a disconnected session must arm the broker-health outage clock")

	// Past the threshold the owner must hand the partition to a healthy standby
	// rather than hold it forever.
	fake.Advance(brokerHealthStepDown + renewInterval)
	wait.RequireReceive(t, sess.closedCh, 3*time.Second)
	wait.Until(t, 3*time.Second, "lease released on broker-path step-down",
		func() bool { return store.releaseCount() >= 1 })
}

// The hand-off that completes a step-down taken under activation returns the
// bare lease-loss sentinel, which would drop the marker that stops this process
// re-seizing the lease it just released — silently turning the escalation back
// into a re-acquire, with nothing to notice.
func TestFinishActivationLeaseLoss_KeepsTheBrokerPathMarker(t *testing.T) {
	cfg := Config{
		SessionID:     "sess-marker",
		Exclusive:     true,
		LeaseTTL:      time.Minute,
		RenewInterval: 5 * time.Second,
		StepDownGrace: time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, newCountingSession(), &transientRenewStore{}, "owner-1", nil,
		&ports.NoopExporter{}, clock.System)

	loss := &activationLeaseLoss{closeCompleted: true}
	got := mgr.finishActivationLeaseLoss(t.Context(),
		fmt.Errorf("%w: %w", errBrokerPathStepDown, loss), true)

	require.ErrorIs(t, got, errBrokerPathStepDown,
		"the broker-path marker must survive the activation-loss hand-off")
	require.ErrorIs(t, got, errLeaseLostAfterRenewal,
		"the lease really did transfer, so the lease-loss signals must still fire")
}

// BrokerHealthStepDown means "an owner RELEASED its lease so a standby could
// take over a node-local broker outage" — that is what an operator alerts on.
// A source Close that ignores its context hands nothing over: the lease is
// deliberately kept until natural expiry so no standby can overlap a still
// subscribed session. Counting that would put a hand-off that never happened on
// the dashboard.
func TestSessionManager_BrokerPathStepDown_CountsNothingWhenTheCloseWedges(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &transientRenewStore{}
	sess := newCountingSession()

	const (
		renewInterval        = 5 * time.Second
		brokerHealthStepDown = 30 * time.Second
	)
	cfg := Config{
		SessionID:                    "sess-wedged-close",
		Exclusive:                    true,
		ConnectAfterLease:            true,
		LeaseTTL:                     10 * time.Minute,
		RenewInterval:                renewInterval,
		RenewJitter:                  0,
		MaxRenewFails:                1,
		StepDownGrace:                20 * time.Millisecond,
		PostAcquireActivationTimeout: 5 * time.Minute,
		BrokerHealthStepDown:         brokerHealthStepDown,
	}
	rec := &ports.RecordingExporter{}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, rec, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()
	t.Cleanup(func() { cancel() })

	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	gate := sess.gateClose()
	t.Cleanup(func() { close(gate) })

	select {
	case sess.events <- ports.SessionEvent{Type: ports.SessionDisconnected}:
	case <-time.After(2 * time.Second):
		t.Fatal("renew loop did not consume the disconnect event")
	}
	wait.Until(t, 2*time.Second, "broker-health outage clock armed", func() bool {
		return !mgr.nonConvergedSince().IsZero()
	})
	fake.Advance(brokerHealthStepDown + renewInterval)

	// Only the bounded-close ceiling can unblock a Close that ignores ctx, and
	// it is driven by the fake clock — wait for it to be armed before firing it.
	waitTimerCount(t, fake, 1, 3*time.Second)
	fake.Advance(cfg.StepDownGrace)

	err := wait.RequireReceive(t, runErr, 10*time.Second)
	require.ErrorIs(t, err, ErrSessionUnrecoverable, "a wedged close is terminal")
	require.Zero(t, store.releaseCount(),
		"a wedged close must keep the lease until natural expiry, not hand it over")
	require.Empty(t, rec.FindEntries(shared.MetricBrokerHealthStepDown),
		"nothing was handed over, so nothing may be counted as a broker-path step-down")
}
