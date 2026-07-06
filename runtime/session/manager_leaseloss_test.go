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

// leaseLossStore is a LeaseStore that succeeds Acquire (always granting the
// lease to the asking owner) and succeeds Renew a configurable number of
// times before failing every subsequent Renew. It drives the renew loop into
// a step-down deterministically: after failRenewAfter successful renews, the
// next renews fail, and once MaxRenewFails consecutive failures accumulate the
// manager steps down. Re-Acquire then succeeds, so the manager re-acquires.
type leaseLossStore struct {
	mu             sync.Mutex
	version        uint64
	owner          string
	renews         int32
	releases       int32
	releasedVers   []uint64
	failRenewAfter int32
	onRenew        chan struct{}
}

func newLeaseLossStore(failRenewAfter int32, onRenew chan struct{}) *leaseLossStore {
	return &leaseLossStore{failRenewAfter: failRenewAfter, onRenew: onRenew}
}

func (s *leaseLossStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.owner = ownerID
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (s *leaseLossStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	n := atomic.AddInt32(&s.renews, 1)
	if s.onRenew != nil {
		s.onRenew <- struct{}{}
	}
	if n > atomic.LoadInt32(&s.failRenewAfter) {
		// Lease taken over by another owner -> renewal must fail.
		return persistence.LeaseToken{}, shared.ErrVersionMismatch
	}
	s.mu.Lock()
	owner := s.owner
	s.mu.Unlock()
	return persistence.LeaseToken{Version: token.Version, Owner: owner}, nil
}

func (s *leaseLossStore) Release(_ context.Context, _ string, token persistence.LeaseToken) error {
	atomic.AddInt32(&s.releases, 1)
	s.mu.Lock()
	s.releasedVers = append(s.releasedVers, token.Version)
	s.mu.Unlock()
	return nil
}

func (s *leaseLossStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: s.owner, Version: s.version}, nil
}

func (s *leaseLossStore) releaseCount() int32 { return atomic.LoadInt32(&s.releases) }

// currentVersion is the version of the most recently acquired lease (the
// re-acquired lease after step-down).
func (s *leaseLossStore) currentVersion() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// wasReleased reports whether the given lease version was ever Released.
func (s *leaseLossStore) wasReleased(v uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rv := range s.releasedVers {
		if rv == v {
			return true
		}
	}
	return false
}

// countingSession is a ports.Session that counts Start/Close calls and exposes
// them over buffered channels so tests can synchronize without polling. Its
// Health reflects the real lifecycle (Connected after Start, not Connected
// after Close) so the deferred re-acquire path (which is Health-gated) behaves
// realistically. The events channel is reopened on Start after a Close so the
// renew loop does not observe a permanently closed channel across terms.
type countingSession struct {
	mu        sync.Mutex
	starts    int
	closes    int
	connected bool
	closed    bool
	events    chan ports.SessionEvent
	startedCh chan int
	closedCh  chan int
}

func newCountingSession() *countingSession {
	return &countingSession{
		events:    make(chan ports.SessionEvent),
		startedCh: make(chan int, 8),
		closedCh:  make(chan int, 8),
	}
}

func (s *countingSession) Start(context.Context) error {
	s.mu.Lock()
	s.starts++
	s.connected = true
	if s.closed {
		// Re-open the events channel for the new term.
		s.events = make(chan ports.SessionEvent)
		s.closed = false
	}
	n := s.starts
	s.mu.Unlock()
	select {
	case s.startedCh <- n:
	default:
	}
	return nil
}

func (s *countingSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }

func (s *countingSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *countingSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *countingSession) Close(context.Context) error {
	s.mu.Lock()
	s.closes++
	s.connected = false
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	n := s.closes
	s.mu.Unlock()
	select {
	case s.closedCh <- n:
	default:
	}
	return nil
}

func (s *countingSession) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// TestSessionManager_LeaseLoss_StopsAndRestartsSession is the regression test
// for finding C3: on lease loss (step-down) an exclusive session manager must
// stop the source receiver so a now-non-owner cannot keep consuming/ACKing
// source messages during failover (split-brain), and it must re-establish the
// receiver when it re-acquires the lease.
//
// It exercises BOTH exclusive paths — connect-after-lease (deferred) and the
// non-deferred path — on a deterministic fake clock, forcing renew failures
// until step-down and then asserting:
//
//	(a) the source session was Closed on step-down   [the regression assertion]
//	(c) the lease was Released as part of step-down
//	(b) the source session was Started again on re-acquire
func TestSessionManager_LeaseLoss_StopsAndRestartsSession(t *testing.T) {
	for _, tc := range []struct {
		name              string
		connectAfterLease bool
	}{
		{name: "non_deferred", connectAfterLease: false},
		{name: "connect_after_lease", connectAfterLease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runLeaseLossScenario(t, tc.connectAfterLease)
		})
	}
}

func runLeaseLossScenario(t *testing.T, connectAfterLease bool) {
	t.Helper()

	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	// Succeed the first renew, then fail every subsequent renew. With
	// MaxRenewFails=1 the second (failing) renew triggers step-down.
	store := newLeaseLossStore(1, renewCh)
	sess := newCountingSession()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:         "sess-leaseloss",
		Exclusive:         true,
		ConnectAfterLease: connectAfterLease,
		LeaseTTL:          5 * time.Second,
		RenewInterval:     renewInterval,
		RenewJitter:       0,
		MaxRenewFails:     1,
		// Real-time (stepDown grace/release use the real clock, not the fake);
		// keep it tiny so the test stays fast.
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

	// The owner must start the source session once (receiver active).
	select {
	case <-sess.startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("source session was not started on lease acquisition")
	}

	// Wait until the renew loop has registered its timer on the fake clock
	// before advancing, so the advance is never lost to a scheduling race.
	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})

	// First renew succeeds (consecutiveFailures resets to 0).
	fake.Advance(renewInterval)
	select {
	case <-renewCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first renew did not fire after advance")
	}

	// Wait for the timer to be reset after the successful renew before the
	// next advance.
	wait.Until(t, 2*time.Second, "renew timer reset", func() bool {
		return fake.TimerCount() >= 1
	})

	// Second renew fails -> consecutiveFailures reaches MaxRenewFails -> the
	// manager steps down.
	fake.Advance(renewInterval)

	// (a) REGRESSION: step-down must Close the source session so a non-owner
	//     stops consuming/ACKing source messages during failover. Before the
	//     fix, the receiver kept running (CloseCalls stayed 0) and this times
	//     out.
	select {
	case <-sess.closedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("source session was NOT closed on lease loss " +
			"(split-brain risk: a non-owner keeps consuming source messages)")
	}

	// (c) The lease must be released as part of step-down.
	wait.Until(t, 3*time.Second, "lease released on step-down", func() bool {
		return store.releaseCount() >= 1
	})

	// (b) On re-acquire, the source session must be started again so the new
	//     owner resumes consuming.
	wait.Until(t, 5*time.Second, "source session restarted on re-acquire", func() bool {
		return sess.startCount() >= 2
	})
}

// singleUseSession models a real single-use transport (the Paho MQTT session,
// adapters/mqtt/transport/paho/acl_session.go): the first Start succeeds, but
// once Close has been called Start returns shared.ErrUnavailable and never
// reconnects. The reusable countingSession above cannot express this, so it
// proves only the in-process reconnect path that reusable transports support.
// This fake proves the production reality: on a single-use transport the
// manager cannot re-establish the session in-process after a step-down, so it
// surfaces the error from Run. The runtime turns that into a terminal state
// and the liveness probe restarts the process with a fresh session (no message
// loss: the broker redelivers and outbox fencing prevents duplicates).
type singleUseSession struct {
	mu        sync.Mutex
	connected bool
	closed    bool
	closes    int
	events    chan ports.SessionEvent
	closedCh  chan int
}

func newSingleUseSession() *singleUseSession {
	return &singleUseSession{
		events:   make(chan ports.SessionEvent),
		closedCh: make(chan int, 8),
	}
}

func (s *singleUseSession) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return shared.ErrUnavailable.
			WithMessage("single-use session is closed; Start not allowed after Close").
			Wrap(shared.ErrTransportClosedPermanently)
	}
	s.connected = true
	return nil
}

func (s *singleUseSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }

func (s *singleUseSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *singleUseSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *singleUseSession) Close(context.Context) error {
	s.mu.Lock()
	s.closes++
	s.connected = false
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	n := s.closes
	s.mu.Unlock()
	select {
	case s.closedCh <- n:
	default:
	}
	return nil
}

// TestSessionManager_SingleUseSession_ReacquireSurfacesError is the test-
// fidelity companion to the lease-loss regression above. Where countingSession
// can be restarted, a single-use transport cannot: after step-down closes it,
// re-acquisition's ensureConnected calls Start, which returns ErrUnavailable.
// The manager must surface that error from Run (rather than silently looping or
// hanging), because the runtime relies on it to enter a terminal state and have
// the orchestrator restart the process. Covers both exclusive paths.
func TestSessionManager_SingleUseSession_ReacquireSurfacesError(t *testing.T) {
	for _, tc := range []struct {
		name              string
		connectAfterLease bool
	}{
		{name: "non_deferred", connectAfterLease: false},
		{name: "connect_after_lease", connectAfterLease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
			renewCh := make(chan struct{}, 8)
			store := newLeaseLossStore(1, renewCh)
			sess := newSingleUseSession()

			const renewInterval = 500 * time.Millisecond
			cfg := Config{
				SessionID:         "sess-singleuse",
				Exclusive:         true,
				ConnectAfterLease: tc.connectAfterLease,
				LeaseTTL:          5 * time.Second,
				RenewInterval:     renewInterval,
				RenewJitter:       0,
				MaxRenewFails:     1,
				StepDownGrace:     20 * time.Millisecond,
			}
			mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr := make(chan error, 1)
			go func() { runErr <- mgr.Run(ctx) }()

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
			// Second renew fails -> step-down closes the single-use session.
			fake.Advance(renewInterval)
			select {
			case <-sess.closedCh:
			case <-time.After(3 * time.Second):
				t.Fatal("single-use session was not closed on step-down")
			}

			// On re-acquire ensureConnected cannot restart a single-use
			// session; Run must surface ErrUnavailable, classified as
			// ErrSessionUnrecoverable so superviseSession escalates to a process
			// restart instead of looping on the zombie (finding C3-CRITICAL).
			select {
			case err := <-runErr:
				if !errors.Is(err, shared.ErrUnavailable) {
					t.Fatalf("expected Run to surface ErrUnavailable on single-use re-acquire, got %v", err)
				}
				if !errors.Is(err, shared.ErrTransportClosedPermanently) {
					t.Fatalf("expected the permanent-closure marker to propagate from the single-use "+
						"Start-after-Close, got %v", err)
				}
				if !errors.Is(err, ErrSessionUnrecoverable) {
					t.Fatalf("expected re-acquire connect failure to be classified ErrSessionUnrecoverable, got %v", err)
				}
				// The just-acquired lease MUST be released on the connect-failure
				// path so a healthy standby takes over immediately instead of the
				// zombie re-seizing it via the store's same-owner fast path. Assert
				// the RE-ACQUIRED lease version specifically is released: step-down
				// already releases the OLD version once, so a weaker releaseCount()>=1
				// check would stay green even if the re-acquire-path release (the
				// actual C3-CRITICAL fix in releaseAndReturn) were removed.
				reacquired := store.currentVersion()
				if !store.wasReleased(reacquired) {
					t.Fatalf("expected the re-acquired lease (version %d) to be released on the "+
						"re-acquire connect failure; released versions=%v (step-down release must not "+
						"mask a missing re-acquire release)", reacquired, store.releasedVers)
				}
				if store.releaseCount() < 2 {
					t.Fatalf("expected both the step-down release and the re-acquire connect-failure "+
						"release, releases=%d", store.releaseCount())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after single-use re-acquire failure " +
					"(a single-use transport cannot reconnect in place; the manager must " +
					"surface the error so the runtime can restart the process)")
			}
		})
	}
}
