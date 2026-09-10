package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// waitTimerCount spins until the fake clock reports at least n active timers, so
// a test can be sure a background goroutine has ARMED its timer before Advance
// fires it (no lost-fire race). No sleeps: it yields via a tight poll that the
// Go scheduler interleaves with the manager goroutine.
func waitTimerCount(t *testing.T, clk *clocktest.Fake, n int, deadline time.Duration) {
	t.Helper()
	wait.Until(t, deadline, "fake clock timer armed", func() bool {
		return clk.TimerCount() >= n
	})
}

// seqLeaseStore is a LeaseStore that always grants the lease and records the
// global-ordering sequence at which the lease was RELEASED, so a test can assert
// the source session was closed BEFORE the lease became releasable.
type seqLeaseStore struct {
	mu         sync.Mutex
	version    uint64
	owner      string
	seq        *atomic.Int64
	releaseSeq int64
	releases   int32
}

func newSeqLeaseStore(seq *atomic.Int64) *seqLeaseStore {
	return &seqLeaseStore{seq: seq}
}

func (s *seqLeaseStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.owner = ownerID
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (s *seqLeaseStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	owner := s.owner
	s.mu.Unlock()
	return persistence.LeaseToken{Version: token.Version, Owner: owner}, nil
}

func (s *seqLeaseStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	s.mu.Lock()
	s.releaseSeq = s.seq.Add(1)
	s.releases++
	s.mu.Unlock()
	return nil
}

func (s *seqLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: s.owner, Version: s.version}, nil
}

func (s *seqLeaseStore) releaseOrder() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseSeq
}

// failReconcileSession models a session whose FIRST (initial) Reconcile succeeds
// and whose SECOND (reconnect-driven) Reconcile FAILS deterministically — an
// ACL/topic-deletion / partial-outage reconcile error. It records the
// global-ordering sequence at which Close was called so a test can assert the
// source session is closed BEFORE the lease is released.
type failReconcileSession struct {
	mu             sync.Mutex
	connected      bool
	closed         bool
	events         chan ports.SessionEvent
	reconcileCalls atomic.Int32
	seq            *atomic.Int64
	closeSeq       int64
	closeCalls     int32
	reconcileErr   error
}

func newFailReconcileSession(seq *atomic.Int64) *failReconcileSession {
	return &failReconcileSession{
		events: make(chan ports.SessionEvent, 1),
		seq:    seq,
	}
}

func (s *failReconcileSession) Start(context.Context) error {
	s.mu.Lock()
	s.connected = true
	if s.closed {
		s.events = make(chan ports.SessionEvent, 1)
		s.closed = false
	}
	ev := s.events
	s.mu.Unlock()
	select {
	case ev <- ports.SessionEvent{Type: ports.SessionConnected}:
	default:
	}
	return nil
}

func (s *failReconcileSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	if s.reconcileCalls.Add(1) == 1 {
		return nil // initial reconcile succeeds
	}
	if s.reconcileErr != nil {
		return s.reconcileErr
	}
	return errors.New("reconcile failed: source topic ACL revoked")
}

func (s *failReconcileSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *failReconcileSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *failReconcileSession) Close(context.Context) error {
	s.mu.Lock()
	s.connected = false
	if s.closeSeq == 0 {
		s.closeSeq = s.seq.Add(1)
	}
	s.closeCalls++
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	s.mu.Unlock()
	return nil
}

func (s *failReconcileSession) closeOrder() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeSeq
}

// TestSessionManager_SessionFailure_ClosesSourceBeforeReleasingLease is the
// regression test: an exclusive session that holds the lease and
// then hits a reconcile-on-reconnect FAILURE must CLOSE the source session
// BEFORE it releases the lease. Otherwise a standby acquires the released lease
// while the old owner is still connected/subscribed and its route receiver keeps
// consuming+acking source messages — split-brain, duplicate egress, source ACK
// by a non-owner.
//
// The assertion is ordering-based: Close's global-ordering sequence must be
// strictly less than the lease Release's sequence, i.e. consuming stops before
// the lease becomes releasable by a standby.
func TestSessionManager_TerminalIngressQuiescenceFailureDoesNotReleaseLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	var seq atomic.Int64
	store := newSeqLeaseStore(&seq)
	sess := newFailReconcileSession(&seq)
	sess.reconcileErr = shared.ErrUnavailable.
		WithMessage("source ingress did not quiesce before recycle").
		Wrap(shared.ErrTransportClosedPermanently)

	mgr := NewWithMetrics(Config{
		SessionID:        "sess-terminal-quiescence",
		Exclusive:        true,
		LeaseTTL:         5 * time.Second,
		RenewInterval:    10 * time.Second,
		RenewCallTimeout: 100 * time.Millisecond,
		MaxRenewFails:    3,
		StepDownGrace:    20 * time.Millisecond,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(context.Background()) }()
	err := wait.RequireReceive(t, runErr, 2*time.Second)
	if !errors.Is(err, ErrSessionUnrecoverable) {
		t.Fatalf("Run error = %v, want ErrSessionUnrecoverable", err)
	}
	if sess.closeOrder() == 0 {
		t.Fatal("terminal quiescence failure did not disconnect the source")
	}
	if got := store.releaseOrder(); got != 0 {
		t.Fatalf("unsafe terminal quiescence failure released lease at sequence %d", got)
	}
	if _, held := mgr.Token(); held {
		t.Fatal("terminal manager still authorizes fenced work after failure")
	}
}

// Mutation: delete the m.closeSourceBounded call in afterRenewLoopExit's
// session-failure branch and Close is never invoked on this path, so closeOrder
// stays 0 and this test fails.
func TestSessionManager_SessionFailure_ClosesSourceBeforeReleasingLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	var seq atomic.Int64
	store := newSeqLeaseStore(&seq)
	sess := newFailReconcileSession(&seq)

	cfg := Config{
		SessionID:        "sess-critical1",
		Exclusive:        true,
		LeaseTTL:         5 * time.Second,
		RenewInterval:    10 * time.Second, // renew timer never fires during the test
		RenewJitter:      0,
		RenewCallTimeout: 100 * time.Millisecond,
		MaxRenewFails:    3,
		StepDownGrace:    20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	// The reconnect reconcile fails deterministically and synchronously, so Run
	// returns the session-failure error without any clock advance.
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected Run to surface the reconcile-failure error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after a deterministic reconcile failure")
	}

	closeAt := sess.closeOrder()
	releaseAt := store.releaseOrder()
	if closeAt == 0 {
		t.Fatal("source session was NEVER closed on the session-failure path; " +
			"a standby can acquire the released lease while this owner keeps consuming (split-brain)")
	}
	if releaseAt == 0 {
		t.Fatal("expected the lease to be released on the session-failure path")
	}
	if closeAt >= releaseAt {
		t.Fatalf("source session closed (seq=%d) at or after the lease was released (seq=%d); "+
			"there is a consume-after-release window in which a standby overlaps a still-subscribed old owner",
			closeAt, releaseAt)
	}
}

// ctxIgnoringReconcileSession models a session whose reconnect-driven Reconcile
// IGNORES context cancellation entirely (a broker SDK call that blocks on an
// internal channel and never consults ctx). The FIRST Reconcile succeeds; the
// SECOND blocks until the test explicitly releases it at cleanup — it never
// observes ctx.Done(), so ONLY the injected-clock hard ceiling can unblock the
// manager.
type ctxIgnoringReconcileSession struct {
	mu             sync.Mutex
	connected      bool
	closed         bool
	events         chan ports.SessionEvent
	reconcileCalls atomic.Int32
	hungEntered    chan struct{}
	release        chan struct{}
	closeCalls     int32
}

func newCtxIgnoringReconcileSession() *ctxIgnoringReconcileSession {
	return &ctxIgnoringReconcileSession{
		events:      make(chan ports.SessionEvent, 1),
		hungEntered: make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
}

func (s *ctxIgnoringReconcileSession) Start(context.Context) error {
	s.mu.Lock()
	s.connected = true
	if s.closed {
		s.events = make(chan ports.SessionEvent, 1)
		s.closed = false
	}
	ev := s.events
	s.mu.Unlock()
	select {
	case ev <- ports.SessionEvent{Type: ports.SessionConnected}:
	default:
	}
	return nil
}

func (s *ctxIgnoringReconcileSession) Reconcile(_ context.Context, _ connectivity.SessionPlan) error {
	if s.reconcileCalls.Add(1) == 1 {
		return nil // initial reconcile succeeds
	}
	select {
	case s.hungEntered <- struct{}{}:
	default:
	}
	// Deliberately does NOT select on ctx.Done(): this adapter ignores context
	// cancellation. Only the test's cleanup can release it.
	<-s.release
	return errors.New("reconcile abandoned after bound")
}

func (s *ctxIgnoringReconcileSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *ctxIgnoringReconcileSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *ctxIgnoringReconcileSession) Close(context.Context) error {
	s.mu.Lock()
	s.connected = false
	atomic.AddInt32(&s.closeCalls, 1)
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	s.mu.Unlock()
	return nil
}

func (s *ctxIgnoringReconcileSession) closeCount() int32 { return atomic.LoadInt32(&s.closeCalls) }

// TestSessionManager_ReconnectReconcile_CtxIgnoringUnblocksAtCeiling is the
// regression test: a reconnect-driven Reconcile that ignores ctx must
// NOT starve lease renewal. handleSessionEvent used to call Reconcile
// SYNCHRONOUSLY, so a ctx-ignoring broker call blocked the renew select loop
// forever: the renewal timer case was never serviced, the local lease expired,
// and a standby seized it while THIS session stayed subscribed (split-brain +
// renewal starvation). After the fix the call is raced against the same
// eventReconcileTimeout ceiling on the injected clock, so the renew loop unblocks
// at the ceiling even though the adapter ignores ctx; the session is then closed
// and the lease is retained until natural expiry because the parked
// Reconcile goroutine may still mutate/send.
//
// Mutation: revert boundedReconcile to a synchronous m.session.Reconcile and the
// renew loop never unblocks (the ceiling timer is never consulted), so Run never
// returns and this test times out.
func TestSessionManager_ReconnectReconcile_CtxIgnoringUnblocksAtCeiling(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil) // never fail a renew
	sess := newCtxIgnoringReconcileSession()
	t.Cleanup(func() { close(sess.release) }) // free the parked ctx-ignoring goroutine

	cfg := Config{
		SessionID:     "sess-high6",
		Exclusive:     true,
		LeaseTTL:      8 * time.Second, // eventReconcileTimeout ceiling = min(RenewCallTimeout, LeaseTTL/4) = 2s
		RenewInterval: 30 * time.Second,
		RenewJitter:   0,
		// A LARGE real-clock RenewCallTimeout so the cooperative (real-ctx) path
		// cannot be what unblocks the loop within the test window — only the
		// injected-clock ceiling (also 2s) can, and it is driven by Advance.
		RenewCallTimeout: 2 * time.Second,
		MaxRenewFails:    3,
		StepDownGrace:    20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	// The ctx-ignoring reconcile must be reached inside boundedReconcile.
	wait.RequireReceive(t, sess.hungEntered, 2*time.Second)

	// Two armed timers: the renew timer (30s) + the boundedReconcile ceiling
	// (2s). Wait for the ceiling to be armed before advancing so Advance fires
	// it deterministically.
	waitTimerCount(t, fake, 2, 2*time.Second)

	// Advance past the ceiling (2s) but not the renew timer (30s): ONLY the
	// reconcile ceiling fires, the renew loop unblocks, and the session is
	// closed without transferring its lease under the still-parked work.
	fake.Advance(2 * time.Second)

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected Run to surface the bounded-reconcile ceiling error, got nil")
		}
		var be *shared.BridgeError
		if !errors.As(err, &be) || be.Code != shared.ErrCodeTimeout {
			t.Fatalf("expected the ceiling timeout to surface as a BridgeError timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the reconcile ceiling fired — a ctx-ignoring " +
			"reconcile starved the renew loop (lease would silently expire; standby overlaps a subscribed owner)")
	}

	wait.Until(t, 2*time.Second, "source session closed on session-failure restart", func() bool {
		return sess.closeCount() >= 1
	})
	if got := store.releaseCount(); got != 0 {
		t.Fatalf("ctx-ignoring reconcile released lease while parked work remained: %d", got)
	}
}

// TestReconnectReconcile_CtxIgnoring_EscalatesTerminal guards reconcile
// escalation. boundedReconcile spawns a fresh goroutine on every reconnect and has no
// pre-spawn in-flight latch (unlike boundedSend's ≤1 sendHung cap). A ctx-ignoring
// Reconcile that only unblocks at the injected-clock ceiling would, if the session
// were merely restarted in place, spawn one parked Reconcile goroutine per broker
// flap — unbounded. The fix escalates the FIRST ceiling-fire (completed == false:
// the adapter ignored ctx and Reconcile is STILL parked) to a terminal
// ErrSessionUnrecoverable, so superviseSession stops restart-and-respawn and the
// pod restart tears the wedged transport down at the OS level — capping parked
// Reconcile goroutines at ONE for the process lifetime.
//
// Mutation: revert boundedReconcile to return the ceiling error UNWRAPPED (drop
// the ErrSessionUnrecoverable escalation) and the ceiling error is a plain
// transient — superviseSession would restart-and-respawn, so
// errors.Is(err, ErrSessionUnrecoverable) is false and this test fails.
func TestReconnectReconcile_CtxIgnoring_EscalatesTerminal(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil) // never fail a renew
	sess := newCtxIgnoringReconcileSession()
	t.Cleanup(func() { close(sess.release) }) // free the parked ctx-ignoring goroutine

	cfg := Config{
		SessionID:        "sess-b5",
		Exclusive:        true,
		LeaseTTL:         8 * time.Second, // ceiling = min(RenewCallTimeout, LeaseTTL/4) = 2s
		RenewInterval:    30 * time.Second,
		RenewJitter:      0,
		RenewCallTimeout: 2 * time.Second,
		MaxRenewFails:    3,
		StepDownGrace:    20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	// The ctx-ignoring reconcile must be reached inside boundedReconcile.
	wait.RequireReceive(t, sess.hungEntered, 2*time.Second)
	// Two armed timers: the renew timer (30s) + the boundedReconcile ceiling (2s).
	waitTimerCount(t, fake, 2, 2*time.Second)
	// Fire ONLY the reconcile ceiling (2s), not the renew timer (30s).
	fake.Advance(2 * time.Second)

	select {
	case err := <-runErr:
		if !errors.Is(err, ErrSessionUnrecoverable) {
			t.Fatalf("a ctx-ignoring reconnect Reconcile must escalate to a terminal "+
				"ErrSessionUnrecoverable so superviseSession stops restart-and-respawn (bounding parked "+
				"Reconcile goroutines at one); got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the reconcile ceiling fired")
	}

	// No restart-in-place respawn: Reconcile was invoked exactly twice — the
	// initial reconcile (runExclusive) plus the one hung reconnect call — so at
	// most ONE Reconcile goroutine is parked.
	if got := sess.reconcileCalls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 Reconcile calls (initial + one hung reconnect), got %d — a "+
			"restart respawned another parked Reconcile goroutine", got)
	}
}

// wedgedCloseSession models a session whose reconnect-driven Reconcile fails
// deterministically (session failure) and whose Close IGNORES ctx — it parks
// until the test releases it at cleanup. This is the exact case the bounded Close
// exists for: the ONLY thing that can unblock the manager is the injected-clock
// ceiling, and when it fires the source is STILL subscribed (Close never
// returned).
type wedgedCloseSession struct {
	mu             sync.Mutex
	connected      bool
	events         chan ports.SessionEvent
	reconcileCalls atomic.Int32
	closeEntered   chan struct{}
	release        chan struct{}
}

func newWedgedCloseSession() *wedgedCloseSession {
	return &wedgedCloseSession{
		events:       make(chan ports.SessionEvent, 1),
		closeEntered: make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
}

func (s *wedgedCloseSession) Start(context.Context) error {
	s.mu.Lock()
	s.connected = true
	ev := s.events
	s.mu.Unlock()
	select {
	case ev <- ports.SessionEvent{Type: ports.SessionConnected}:
	default:
	}
	return nil
}

func (s *wedgedCloseSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	if s.reconcileCalls.Add(1) == 1 {
		return nil // initial reconcile succeeds
	}
	return errors.New("reconcile failed on reconnect: source topic ACL revoked")
}

func (s *wedgedCloseSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *wedgedCloseSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// Close deliberately does NOT consult ctx: it parks until the test releases it.
// The manager can only be unblocked by the closeSourceBounded hard ceiling.
func (s *wedgedCloseSession) Close(context.Context) error {
	select {
	case s.closeEntered <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

// TestSessionManager_SessionFailure_WedgedCloseDoesNotReleaseLease is the
// regression test: when the bounded source Close IGNORES ctx and only the
// hard ceiling unblocks the manager, the source is STILL subscribed, so the lease
// MUST NOT be released — releasing it would let a standby acquire and overlap a
// still-consuming old owner, re-opening the very split-brain closes.
// Instead the manager escalates to a terminal ErrSessionUnrecoverable so the pod
// restart forcibly tears down the wedged transport at the OS level; the lease
// stays held and expires only by natural TTL, preserving single-owner.
//
// Mutation: make closeSourceBounded always report completed==true (or drop the
// `if !closed` guard and release unconditionally) and the lease IS released while
// the source stays subscribed — releaseCount()>0 and Run returns the plain
// reconcile error (not ErrSessionUnrecoverable) — so this test fails.
func TestSessionManager_SessionFailure_WedgedCloseDoesNotReleaseLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(1<<30, nil) // always grant; never fail a renew
	sess := newWedgedCloseSession()
	t.Cleanup(func() { close(sess.release) }) // free the parked ctx-ignoring Close

	cfg := Config{
		SessionID:        "sess-b1",
		Exclusive:        true,
		LeaseTTL:         8 * time.Second,
		RenewInterval:    30 * time.Second, // renew timer never fires during the test
		RenewJitter:      0,
		RenewCallTimeout: 2 * time.Second,
		MaxRenewFails:    3,
		// closeSourceBounded ceiling == releaseTimeout == StepDownGrace (<=5s).
		StepDownGrace: 20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	// The reconnect reconcile fails synchronously; afterRenewLoopExit then enters
	// the bounded Close, which parks ignoring ctx.
	wait.RequireReceive(t, sess.closeEntered, 2*time.Second)

	// Wait for the closeSourceBounded ceiling to be armed on the fake clock, then
	// fire ONLY it (past the 20ms releaseTimeout).
	waitTimerCount(t, fake, 1, 2*time.Second)
	fake.Advance(20 * time.Millisecond)

	select {
	case err := <-runErr:
		if !errors.Is(err, ErrSessionUnrecoverable) {
			t.Fatalf("a wedged (ctx-ignoring) Close must escalate to terminal ErrSessionUnrecoverable, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the bounded-close ceiling fired")
	}

	// The lease must NOT have been released: a wedged-but-subscribed session
	// cannot be handed off to a standby without split-brain.
	if n := store.releaseCount(); n != 0 {
		t.Fatalf("lease released %d time(s) after a WEDGED source Close; a standby can now overlap a "+
			"still-subscribed old owner (split-brain). The lease must stay held and expire only by TTL.", n)
	}
}
