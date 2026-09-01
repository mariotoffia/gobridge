package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// singleUseReconnectFailSession models the production default pairing: a
// SINGLE-USE transport (the Paho MQTT session — Start is refused after Close
// with the permanent-closure marker) whose first Start+Reconcile succeed and
// whose reconnect-driven Reconcile then FAILS, i.e. a session failure while the
// lease is held.
//
// The existing singleUseSession proves only the step-down/re-acquire path; it
// never fails a reconcile, so it cannot reach the session-failure recovery
// branch that closes the source, releases the lease and restarts Run.
type singleUseReconnectFailSession struct {
	mu             sync.Mutex
	connected      bool
	closed         bool
	events         chan ports.SessionEvent
	reconcileCalls atomic.Int32
	startCalls     atomic.Int32
	closeCalls     atomic.Int32
	// wedgeClose parks Close ignoring ctx once armed, modelling a transport whose
	// teardown never returns. closeEntered signals that the park was reached.
	wedgeClose   atomic.Bool
	closeEntered chan struct{}
	release      chan struct{}
}

func newSingleUseReconnectFailSession() *singleUseReconnectFailSession {
	return &singleUseReconnectFailSession{
		events:       make(chan ports.SessionEvent, 1),
		closeEntered: make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
}

func (s *singleUseReconnectFailSession) Start(context.Context) error {
	s.startCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return shared.ErrUnavailable.
			WithMessage("single-use session is closed; Start not allowed after Close").
			Wrap(shared.ErrTransportClosedPermanently)
	}
	s.connected = true
	// Queue the reconnect edge the manager reconciles on; the renew loop only
	// binds the events channel once activation has converged, so the buffered
	// event is consumed immediately after the initial Reconcile succeeds.
	select {
	case s.events <- ports.SessionEvent{Type: ports.SessionConnected}:
	default:
	}
	return nil
}

func (s *singleUseReconnectFailSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	if s.reconcileCalls.Add(1) == 1 {
		return nil
	}
	return errors.New("reconcile failed on reconnect: source topic ACL revoked")
}

func (s *singleUseReconnectFailSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *singleUseReconnectFailSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *singleUseReconnectFailSession) Close(context.Context) error {
	s.closeCalls.Add(1)
	if s.wedgeClose.Load() {
		select {
		case s.closeEntered <- struct{}{}:
		default:
		}
		<-s.release
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = false
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	return nil
}

var _ ports.Session = (*singleUseReconnectFailSession)(nil)

// TestSessionManager_DeferredConnect_SessionFailureRestartReleasesReseizedLease
// pins the failover hand-off on the DEFAULT deferred-connect profile
// (connect_after_lease, a single-use MQTT transport, a real lease store).
//
// A session failure closes the source, releases the lease and returns for an
// isolated restart. The supervisor restarts Run on the SAME manager and the SAME
// dead session; that fresh Run re-seizes the lease through the store's same-owner
// fast path and only then discovers that the single-use transport refuses
// Start-after-Close. That failure is terminal — nothing can re-establish the
// session in this process — but nothing was accepted either: no subscription, no
// delivery, no unsettled work. The lease MUST therefore be released so a standby
// takes over within one acquire poll instead of waiting out the full lease TTL
// while a provably dead owner holds a freshly renewed row.
//
// Mutation: leave the deferred first-connect branch non-escalatable and
// releaseAndReturn keeps the re-seized lease (releases == 1, the re-acquired
// version never released) — takeover is delayed to lease_ttl and this test fails.
func TestSessionManager_DeferredConnect_SessionFailureRestartReleasesReseizedLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil) // always grant; never fail a renew
	sess := newSingleUseReconnectFailSession()

	mgr := NewWithMetrics(Config{
		SessionID:         "sess-deferred-handoff",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          5 * time.Second,
		RenewInterval:     10 * time.Second, // the renew timer never fires in this test
		RenewJitter:       0,
		RenewCallTimeout:  100 * time.Millisecond,
		MaxRenewFails:     3,
		StepDownGrace:     20 * time.Millisecond,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Term 1: acquire, connect, reconcile, then fail the reconnect reconcile.
	first := make(chan error, 1)
	go func() { first <- mgr.Run(ctx) }()
	firstErr := wait.RequireReceive(t, first, 3*time.Second)
	if firstErr == nil {
		t.Fatal("expected the reconnect-reconcile failure to be surfaced for an isolated restart")
	}
	if errors.Is(firstErr, ErrSessionUnrecoverable) {
		t.Fatalf("a cooperative reconcile failure must stay an isolated restart, got %v", firstErr)
	}
	firstVersion := store.currentVersion()
	if !store.wasReleased(firstVersion) {
		t.Fatalf("session-failure recovery must release the held lease (version %d), released=%v",
			firstVersion, store.releasedVers)
	}

	// Term 2: the supervisor restarts the SAME manager. The lease is re-seized
	// before the dead single-use transport refuses Start.
	second := make(chan error, 1)
	go func() { second <- mgr.Run(ctx) }()
	secondErr := wait.RequireReceive(t, second, 3*time.Second)

	if !errors.Is(secondErr, ErrSessionUnrecoverable) {
		t.Fatalf("Start-after-Close on a single-use transport must escalate to a process restart, got %v", secondErr)
	}
	if !errors.Is(secondErr, shared.ErrTransportClosedPermanently) {
		t.Fatalf("the permanent-closure marker must propagate, got %v", secondErr)
	}

	reseized := store.currentVersion()
	if reseized == firstVersion {
		t.Fatalf("the restarted Run did not re-acquire the lease (version still %d)", reseized)
	}
	if !store.wasReleased(reseized) {
		t.Fatalf("the re-seized lease (version %d) must be released before the process restarts: nothing "+
			"was accepted on the failed deferred connect, so a standby must take over within one acquire "+
			"poll instead of waiting out the full lease TTL; released versions=%v",
			reseized, store.releasedVers)
	}
	if n := store.releaseCount(); n != 2 {
		t.Fatalf("expected exactly the session-failure release and the restart release, got %d", n)
	}
	if got := sess.startCalls.Load(); got != 2 {
		t.Fatalf("expected the restarted Run to attempt Start on the dead session, start calls=%d", got)
	}
}

// TestSessionManager_DeferredConnect_WedgedCloseKeepsReseizedLease is the
// negative control for the hand-off above. Releasing the re-seized lease is
// justified by "this transport is permanently closed, so nothing of ours can
// still send" — and a session can latch that permanent marker ASYNCHRONOUSLY
// while accepted deliveries are still settling (the paho session's
// ingress-poison rejection returns at once and quiesces on a goroutine). So the
// marker alone is not evidence: the source is closed (bounded) first, and when
// that close never returns the lease is KEPT and the term goes terminal. The
// pod restart then tears the wedged transport down at the OS level and the
// standby takes over at natural TTL.
//
// Mutation: release unconditionally on the escalation path and the lease is
// handed to a standby while a still-subscribed session may send — this test
// fails on `released versions=[1 2]`.
func TestSessionManager_DeferredConnect_WedgedCloseKeepsReseizedLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil)
	sess := newSingleUseReconnectFailSession()
	t.Cleanup(func() { close(sess.release) })

	mgr := NewWithMetrics(Config{
		SessionID:         "sess-deferred-wedge",
		Exclusive:         true,
		ConnectAfterLease: true,
		LeaseTTL:          5 * time.Second,
		RenewInterval:     10 * time.Second,
		RenewJitter:       0,
		RenewCallTimeout:  100 * time.Millisecond,
		MaxRenewFails:     3,
		// The bounded-close ceiling equals releaseTimeout == StepDownGrace.
		StepDownGrace: 20 * time.Millisecond,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan error, 1)
	go func() { first <- mgr.Run(ctx) }()
	require.Error(t, wait.RequireReceive(t, first, 3*time.Second))
	firstVersion := store.currentVersion()
	require.True(t, store.wasReleased(firstVersion), "the session-failure release must still happen")

	// The restarted term finds a permanently closed transport, and this time the
	// proving Close parks ignoring ctx.
	sess.wedgeClose.Store(true)
	second := make(chan error, 1)
	go func() { second <- mgr.Run(ctx) }()

	wait.RequireReceive(t, sess.closeEntered, 3*time.Second)
	wait.Until(t, 3*time.Second, "bounded-close ceiling armed", func() bool { return fake.TimerCount() >= 1 })
	fake.Advance(20 * time.Millisecond)

	err := wait.RequireReceive(t, second, 3*time.Second)
	require.ErrorIs(t, err, ErrSessionUnrecoverable)

	reseized := store.currentVersion()
	require.NotEqual(t, firstVersion, reseized, "precondition: the restarted term must re-acquire")
	assert.False(t, store.wasReleased(reseized),
		"a source whose Close never returned may still be sending: the re-seized lease must expire by "+
			"TTL after the pod restart, not be handed to a standby; released versions=%v", store.releasedVers)
}
