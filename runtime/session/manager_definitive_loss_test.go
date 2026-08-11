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

// TestSessionManager_DefinitiveLeaseLoss_StepsDownImmediately is the
// regression test for a Renew error that PROVES the lease is no
// longer ours (stale fencing token, not-found, version mismatch) must bypass
// the MaxRenewFails consecutive-failure counter and step down after the FIRST
// such error. Before the fix the manager treated definitive signals like
// transient ones and kept consuming alongside the new owner for
// MaxRenewFails-1 additional renew intervals (~33s HA / ~220s defaults of
// dual-active consumption).
//
// The test pins MaxRenewFails=3 and fails renew #1 with a definitive error
// (leaseLossStore returns shared.ErrVersionMismatch). Only ONE renew tick is
// driven on the fake clock; if the counter path were taken the manager would
// wait for two more renew intervals that never come, and the step-down
// assertions below would time out.
func TestSessionManager_DefinitiveLeaseLoss_StepsDownImmediately(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	// failRenewAfter=0: the very first renew fails with ErrVersionMismatch —
	// a definitive lease-loss signal.
	store := newLeaseLossStore(0, renewCh)
	sess := newCountingSession()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:     "sess-definitive-loss",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		// The heart of the regression: three tolerated transient failures. A
		// definitive signal must NOT consume this budget.
		MaxRenewFails: 3,
		// Step-down grace runs on the real clock; keep it tiny.
		StepDownGrace: 20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

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
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})

	// Drive exactly ONE renew tick: it fails definitively.
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)

	// Step-down must follow from that single definitive failure — the source
	// session is closed so a fenced-out owner stops consuming immediately.
	// With the bug (definitive treated as transient) this times out: the fake
	// clock never advances again, so renews #2 and #3 never fire.
	select {
	case <-sess.closedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("session was NOT closed after a definitive lease-loss signal: " +
			"the manager is burning MaxRenewFails renew intervals while a new " +
			"owner consumes in parallel (dual-active window)")
	}

	// The lease row is released as part of step-down.
	wait.Until(t, 3*time.Second, "lease released on definitive step-down", func() bool {
		return store.releaseCount() >= 1
	})
}

// deadlineRecordingLeaseStore records whether each Acquire/Renew call carried
// a context deadline. It grants every Acquire and renews successfully so the
// manager's renew loop keeps running.
type deadlineRecordingLeaseStore struct {
	mu               sync.Mutex
	version          uint64
	acquireDeadlines []bool
	renewDeadlines   []bool
	onRenew          chan struct{}
}

func (s *deadlineRecordingLeaseStore) Acquire(ctx context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.acquireDeadlines = append(s.acquireDeadlines, hasDeadline)
	s.version++
	v := s.version
	s.mu.Unlock()
	return persistence.LeaseToken{Version: v, Owner: ownerID}, nil
}

func (s *deadlineRecordingLeaseStore) Renew(ctx context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.renewDeadlines = append(s.renewDeadlines, hasDeadline)
	s.mu.Unlock()
	if s.onRenew != nil {
		s.onRenew <- struct{}{}
	}
	return token, nil
}

func (s *deadlineRecordingLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	return nil
}

func (s *deadlineRecordingLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.LeaseInfo{LeaseID: leaseID, Version: s.version}, nil
}

func (s *deadlineRecordingLeaseStore) snapshot() (acquire, renew []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.acquireDeadlines...), append([]bool(nil), s.renewDeadlines...)
}

// TestSessionManager_LeaseCalls_CarryPerCallDeadline is the regression test
// for every lease-store Acquire and Renew call must carry a
// per-call context deadline (derived RenewCallTimeout = min(RenewInterval/2,
// 5s), floored at 1s) so a hung backend cannot stretch step-down and takeover
// unboundedly. It asserts the deadline at the STORE side, proving the timeout
// is applied at the call sites, not merely derived into a field.
func TestSessionManager_LeaseCalls_CarryPerCallDeadline(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	store := &deadlineRecordingLeaseStore{onRenew: renewCh}
	sess := newCountingSession()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:     "sess-call-deadline",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 3,
		StepDownGrace: 20 * time.Millisecond,
		// RenewCallTimeout deliberately left zero: the derived value must
		// still be applied per call.
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

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
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)

	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)

	acquires, renews := store.snapshot()
	if len(acquires) == 0 {
		t.Fatal("no Acquire call recorded")
	}
	for i, has := range acquires {
		if !has {
			t.Fatalf("Acquire call %d carried NO context deadline: a hung lease "+
				"store call can stall takeover unboundedly", i)
		}
	}
	if len(renews) == 0 {
		t.Fatal("no Renew call recorded")
	}
	for i, has := range renews {
		if !has {
			t.Fatalf("Renew call %d carried NO context deadline: a hung lease "+
				"store call can stall step-down unboundedly", i)
		}
	}
}

// eventsClosableSession is a ports.Session whose Events channel the TEST can
// close directly, modelling an unexpected in-transport death of the event
// pump (NOT driven by Close). countingSession cannot express this: it closes
// its events channel only from Close.
type eventsClosableSession struct {
	mu        sync.Mutex
	connected bool
	events    chan ports.SessionEvent
	startedCh chan struct{}
}

func newEventsClosableSession() *eventsClosableSession {
	return &eventsClosableSession{
		events:    make(chan ports.SessionEvent),
		startedCh: make(chan struct{}, 4),
	}
}

func (s *eventsClosableSession) Start(context.Context) error {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	select {
	case s.startedCh <- struct{}{}:
	default:
	}
	return nil
}

func (s *eventsClosableSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}

func (s *eventsClosableSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *eventsClosableSession) Events() <-chan ports.SessionEvent { return s.events }

func (s *eventsClosableSession) Close(context.Context) error {
	s.mu.Lock()
	s.connected = false
	s.mu.Unlock()
	return nil
}

func (s *eventsClosableSession) closeEvents() { close(s.events) }

// TestSessionManager_SessionFailure_ReleasesOwnLease is the regression test
// for finding (release-then-reacquire) on the session-failure exit path:
// when renewLoop exits with a session failure (here: the Events channel closes
// unexpectedly while ctx is still live), the manager must
// RELEASE the lease it still holds before surfacing the error. Before the fix
// the unexpired lease was left in place, so superviseSession's restarted Run
// blocked in Acquire against the manager's OWN lease until LeaseTTL
// self-expiry (up to 360s with defaults) — a self-inflicted outage.
func TestSessionManager_SessionFailure_ReleasesOwnLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	// Renews never fail; the exit is driven purely by the events-channel close.
	store := newLeaseLossStore(1<<30, nil)
	sess := newEventsClosableSession()

	cfg := Config{
		SessionID:     "sess-events-closed",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: 10 * time.Second, // never reached on the fake clock
		RenewJitter:   0,
		MaxRenewFails: 3,
		StepDownGrace: 20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	select {
	case <-sess.startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("session was not started")
	}
	wait.Until(t, 2*time.Second, "lease acquired", func() bool {
		_, held := mgr.Token()
		return held
	})

	// The transport's event pump dies: Events closes while ctx is live.
	sess.closeEvents()

	// Run must SURFACE the failure — not treat it as a clean stop or a
	// lease loss — so superviseSession restarts the session in isolation.
	var err error
	select {
	case err = <-runErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the Events channel closed unexpectedly " +
			"(the session would die silently with no restart)")
	}
	if !errors.Is(err, errSessionEventsClosed) {
		t.Fatalf("expected errSessionEventsClosed, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("test invariant broken: ctx must still be live")
	}

	// The still-held lease must have been released so a restarted Run
	// re-acquires immediately instead of blocking until LeaseTTL self-expiry.
	if store.releaseCount() < 1 {
		t.Fatal("lease was NOT released on the session-failure exit: a restarted " +
			"Run blocks in Acquire against our own unexpired lease until TTL " +
			"self-expiry")
	}
	if _, held := mgr.Token(); held {
		t.Fatal("hasLease must be cleared on the session-failure exit")
	}

	// The definitive-loss signal for observers is ReconcileFailed — not Lost —
	// so a session failure is never mis-observed as a lease transfer.
	sawReconcileFailed := false
	for {
		select {
		case ev := <-mgr.LeaseStateChanged():
			if ev.State == LeaseStateReconcileFailed {
				sawReconcileFailed = true
			}
			if ev.State == LeaseStateLost {
				t.Fatal("session failure must emit LeaseStateReconcileFailed, not LeaseStateLost")
			}
			continue
		default:
		}
		break
	}
	if !sawReconcileFailed {
		t.Fatal("expected a LeaseStateReconcileFailed event on the session-failure exit")
	}
}

// releaseTimestampLeaseStore grants Acquire, fails every Renew transiently
// (shared.ErrUnavailable — NOT definitive), and records the wall-clock instant
// of each Release. Used to verify the step-down grace window is not aborted
// by caller cancellation.
type releaseTimestampLeaseStore struct {
	mu         sync.Mutex
	version    uint64
	releasedAt chan time.Time
	onRenew    chan struct{}
}

func (s *releaseTimestampLeaseStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	s.version++
	v := s.version
	s.mu.Unlock()
	return persistence.LeaseToken{Version: v, Owner: ownerID}, nil
}

func (s *releaseTimestampLeaseStore) Renew(context.Context, string, persistence.LeaseToken, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	if s.onRenew != nil {
		s.onRenew <- struct{}{}
	}
	return persistence.LeaseToken{}, shared.ErrUnavailable.WithMessage("transient store outage")
}

func (s *releaseTimestampLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	select {
	case s.releasedAt <- time.Now():
	default:
	}
	return nil
}

func (s *releaseTimestampLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{LeaseID: leaseID}, nil
}

// TestSessionManager_StepDownGrace_NotAbortedByShutdown is the regression test
// for stepDown's grace window gives in-flight outbox
// Send+Complete a full settle window and "must not be aborted by caller
// cancellation" (its documented contract). Before the fix the grace select
// listened on ctx.Done(), so a shutdown racing a step-down skipped the grace
// and released immediately — the new owner then re-sent the still-in-flight
// work as fenced duplicates.
//
// The test forces a step-down (one transient renew failure with
// MaxRenewFails=1), cancels the caller's ctx the moment the step-down begins
// (observed via the SteppedDown lease event), and asserts the Release still
// happens no EARLIER than the grace window. The lower-bound assertion is
// CI-safe: correct code cannot release before the detached grace timer fires;
// only the buggy early-release path can produce a small elapsed time.
func TestSessionManager_StepDownGrace_NotAbortedByShutdown(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	store := &releaseTimestampLeaseStore{
		releasedAt: make(chan time.Time, 4),
		onRenew:    renewCh,
	}
	sess := newCountingSession()

	const (
		renewInterval = 500 * time.Millisecond
		// Real-clock grace (stepDown runs it on the real clock by design —
		// it bounds real I/O settling). Large enough that the buggy
		// skip-grace path is unambiguous, small enough to keep the test fast.
		grace = 400 * time.Millisecond
		// Margin absorbs channel-handoff delay between the step-down event
		// and the test's timestamp.
		minElapsed = 300 * time.Millisecond
	)
	cfg := Config{
		SessionID:     "sess-grace-shutdown",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		StepDownGrace: grace,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()

	select {
	case <-sess.startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("session was not started")
	}
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})

	// One failing renew crosses MaxRenewFails=1 and starts the step-down.
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)

	// The SteppedDown event is pushed BEFORE the grace wait begins; the source
	// close signal follows it. Use the close signal as the "grace started"
	// marker, then immediately cancel the caller — the exact race covers.
	wait.RequireReceive(t, sess.closedCh, 3*time.Second)
	graceStart := time.Now()
	cancel()

	released := wait.RequireReceive(t, store.releasedAt, 3*time.Second)
	if elapsed := released.Sub(graceStart); elapsed < minElapsed {
		t.Fatalf("lease released %s after step-down began, before the %s grace "+
			"window elapsed: caller cancellation aborted the settle window, so "+
			"in-flight outbox Send+Complete become fenced duplicates",
			elapsed, grace)
	}

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

// TestSessionManager_LeaseEventOverflow_EvictsOldestAndCounts is the
// regression test for when the LeaseStateChanged buffer is full,
// pushLeaseEvent must evict the OLDEST buffered event (keeping the freshest
// transitions for a late consumer) and count the eviction — never silently
// drop the newest event.
func TestSessionManager_LeaseEventOverflow_EvictsOldestAndCounts(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg := Config{SessionID: "sess-event-overflow", Exclusive: true}
	mgr := NewWithMetrics(cfg, newCountingSession(), newLeaseLossStore(0, nil), "owner-1",
		nil, &ports.NoopExporter{}, clock.Clock(fake))

	const extra = 8
	for i := 0; i < leaseEventBuffer+extra; i++ {
		mgr.pushLeaseEvent(LeaseStateRenewed, persistence.LeaseToken{Version: uint64(i + 1)}, nil)
	}

	if drops := mgr.LeaseEventDrops(); drops != extra {
		t.Fatalf("expected %d evictions counted, got %d (overflow "+
			"must be observable, not silent)", extra, drops)
	}

	// The oldest events were evicted: the first buffered event is now the
	// (extra+1)-th push, and the newest push is present at the tail.
	first := <-mgr.LeaseStateChanged()
	if first.Token.Version != extra+1 {
		t.Fatalf("expected oldest surviving event version %d, got %d (evict-oldest "+
			"must preserve the freshest transitions)", extra+1, first.Token.Version)
	}
	var last LeaseStateEvent
	for {
		select {
		case ev := <-mgr.LeaseStateChanged():
			last = ev
			continue
		default:
		}
		break
	}
	if last.Token.Version != leaseEventBuffer+extra {
		t.Fatalf("newest event must never be dropped: expected version %d at the "+
			"tail, got %d", leaseEventBuffer+extra, last.Token.Version)
	}
}

// TestEscalatesToUnrecoverable pins the finding SR-B/N-1 escalation predicate: a
// connect failure escalates to a terminal ErrSessionUnrecoverable (release lease +
// restart pod) ONLY when it carries the permanent shared.ErrTransportClosedPermanently
// marker AND the path is escalatable (a re-acquire reconnect). A plain transient
// ErrUnavailable — which is ALSO a multi-use transport's momentary "broker
// unreachable" — must NOT escalate, so a future multi-use exclusive transport keeps
// recovering via capped-backoff retry instead of a needless process restart.
//
// Before the fix the gate matched the broad shared.ErrUnavailable, so the
// second case below would wrongly escalate.
func TestEscalatesToUnrecoverable(t *testing.T) {
	// Model the real single-use Start-after-Close error: transient ErrUnavailable
	// WRAPPING the permanent marker.
	permanentClosure := shared.ErrUnavailable.
		WithMessage("single-use session closed").
		Wrap(shared.ErrTransportClosedPermanently)

	tests := []struct {
		name        string
		err         error
		escalatable bool
		want        bool
	}{
		{
			name:        "permanent closure on escalatable path escalates",
			err:         permanentClosure,
			escalatable: true,
			want:        true,
		},
		{
			// The KEY new guarantee: a transient broker blip on the re-acquire
			// path stays a retry, never a terminal restart.
			name:        "transient ErrUnavailable on escalatable path does NOT escalate",
			err:         shared.ErrUnavailable.WithMessage("broker momentarily unreachable"),
			escalatable: true,
			want:        false,
		},
		{
			name:        "permanent closure on non-escalatable first-connect path does NOT escalate",
			err:         permanentClosure,
			escalatable: false,
			want:        false,
		},
		{
			name:        "nil error never escalates",
			err:         nil,
			escalatable: true,
			want:        false,
		},
		{
			name:        "unrelated transient error does not escalate",
			err:         shared.ErrConnectionLost,
			escalatable: true,
			want:        false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := escalatesToUnrecoverable(tc.err, tc.escalatable); got != tc.want {
				t.Fatalf("escalatesToUnrecoverable(%v, escalatable=%v) = %v, want %v",
					tc.err, tc.escalatable, got, tc.want)
			}
		})
	}
}

type singleUseTerminalSession struct {
	starts  atomic.Int32
	events  chan ports.SessionEvent
	started chan struct{}
}

func newSingleUseTerminalSession() *singleUseTerminalSession {
	return &singleUseTerminalSession{
		events:  make(chan ports.SessionEvent),
		started: make(chan struct{}, 1),
	}
}

func (s *singleUseTerminalSession) Start(context.Context) error {
	if s.starts.Add(1) > 1 {
		return shared.ErrUnavailable.WithMessage("single-use terminal session").
			Wrap(shared.ErrTransportClosedPermanently)
	}
	s.started <- struct{}{}
	return nil
}
func (*singleUseTerminalSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}
func (*singleUseTerminalSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{}
}
func (s *singleUseTerminalSession) Events() <-chan ports.SessionEvent { return s.events }
func (*singleUseTerminalSession) Close(context.Context) error         { return nil }

func TestSessionManager_TerminalSignalRestartsThenEscalatesSingleUseSession(t *testing.T) {
	sess := newSingleUseTerminalSession()
	mgr := NewWithMetrics(Config{SessionID: "single-use-terminal"}, sess, nil, "owner", nil, &ports.NoopExporter{}, clock.System)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	firstRun := make(chan error, 1)
	go func() { firstRun <- mgr.Run(ctx) }()
	wait.RequireReceive(t, sess.started, time.Second)
	close(sess.events)

	firstErr := wait.RequireReceive(t, firstRun, time.Second)
	if !errors.Is(firstErr, errSessionEventsClosed) {
		t.Fatalf("first Run error = %v, want events-closed failure", firstErr)
	}
	secondErr := mgr.Run(ctx)
	if !errors.Is(secondErr, ErrSessionUnrecoverable) || !errors.Is(secondErr, shared.ErrTransportClosedPermanently) {
		t.Fatalf("second Run error = %v, want single-use terminal escalation", secondErr)
	}
	if got := sess.starts.Load(); got != 2 {
		t.Fatalf("Start calls = %d, want 2", got)
	}
}
