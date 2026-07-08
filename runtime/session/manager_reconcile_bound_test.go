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
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// hungReconcileSession models a single-owner MQTT session whose reconnect-driven
// Reconcile stalls (broker answers keepalive but never completes SUBACK). The
// FIRST Reconcile — the initial one in runExclusive — succeeds; the SECOND —
// driven by the SessionConnected event the renew loop consumes — blocks until
// its context is cancelled (it honors ctx, exactly as paho's ConnectionManager
// Subscribe does). Without the F1 bound the renew select loop would block here
// forever and the lease would silently expire.
type hungReconcileSession struct {
	mu             sync.Mutex
	connected      bool
	closed         bool
	events         chan ports.SessionEvent
	reconcileCalls atomic.Int32
	hungEntered    chan struct{}
}

func newHungReconcileSession() *hungReconcileSession {
	return &hungReconcileSession{
		events:      make(chan ports.SessionEvent, 1),
		hungEntered: make(chan struct{}, 1),
	}
}

func (s *hungReconcileSession) Start(context.Context) error {
	s.mu.Lock()
	s.connected = true
	if s.closed {
		s.events = make(chan ports.SessionEvent, 1)
		s.closed = false
	}
	ev := s.events
	s.mu.Unlock()
	// Emit a reconnect event so the renew loop's event branch fires an
	// event-driven Reconcile (the call F1 bounds).
	select {
	case ev <- ports.SessionEvent{Type: ports.SessionConnected}:
	default:
	}
	return nil
}

func (s *hungReconcileSession) Reconcile(ctx context.Context, _ connectivity.SessionPlan) error {
	if s.reconcileCalls.Add(1) == 1 {
		return nil // initial reconcile succeeds
	}
	select {
	case s.hungEntered <- struct{}{}:
	default:
	}
	<-ctx.Done() // stalled SUBACK; unblocks only when the F1 bound cancels ctx
	return ctx.Err()
}

func (s *hungReconcileSession) Health(context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *hungReconcileSession) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *hungReconcileSession) Close(context.Context) error {
	s.mu.Lock()
	s.connected = false
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	s.mu.Unlock()
	return nil
}

// TestSessionManager_RenewLoop_HungReconcileDoesNotStarveRenewal is the
// regression test for finding F1: a reconnect-driven Reconcile that never
// completes must NOT block the renew select loop indefinitely. Before the fix
// the loop blocks in handleSessionEvent -> Reconcile forever, the renew timer
// never fires, and the lease silently expires while the broker-resumed
// subscription keeps delivering (two live consumers). After the fix the call is
// bounded at min(RenewCallTimeout, LeaseTTL/4); on timeout the error is surfaced
// and the session is restarted with the lease released.
func TestSessionManager_RenewLoop_HungReconcileDoesNotStarveRenewal(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	// Never fail a renew: the ONLY thing that must free the loop is the F1
	// reconcile bound, not a renew failure.
	store := newLeaseLossStore(1<<30, nil)
	sess := newHungReconcileSession()

	cfg := Config{
		SessionID:     "sess-f1",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: 500 * time.Millisecond,
		RenewJitter:   0,
		// eventReconcileTimeout = min(RenewCallTimeout, LeaseTTL/4) = 50ms; the
		// hung reconcile is cut off after 50ms of REAL time (context deadlines
		// are real-clock), so the test needs no fake-clock advance.
		RenewCallTimeout: 50 * time.Millisecond,
		MaxRenewFails:    3,
		StepDownGrace:    20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	// The event-driven (hung) Reconcile must be reached.
	wait.RequireReceive(t, sess.hungEntered, 2*time.Second)

	// F1: the bound cuts off the hung reconcile; Run returns (session-failure
	// restart path) rather than hanging forever, and the lease is released.
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected Run to surface the bounded-reconcile error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected the reconcile bound (context deadline) to surface, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return: a hung reconcile starved the renew loop " +
			"(F1: lease would silently expire while the broker-resumed subscription keeps delivering)")
	}
	wait.Until(t, 2*time.Second, "lease released on session-failure restart", func() bool {
		return store.releaseCount() >= 1
	})
}
