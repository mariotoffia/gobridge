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
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type postAcquireBoundaryClock struct {
	*clocktest.Fake
}

// Leave two nanoseconds between the logical safe deadline and the injected
// hard-stop timer. The tests can therefore exercise the manager's exact
// completion-time comparison deterministically rather than racing two ready
// channels at the same instant.
func (c *postAcquireBoundaryClock) NewTimer(d time.Duration) clock.Timer {
	return c.Fake.NewTimer(d + 2*time.Nanosecond)
}

func (c *postAcquireBoundaryClock) After(d time.Duration) <-chan time.Time {
	return c.NewTimer(d).C()
}

type postAcquireDeadlineStore struct {
	clk                    *clocktest.Fake
	acquireElapsed         time.Duration
	releases               atomic.Int32
	connected              *atomic.Bool
	releasedWhileConnected atomic.Bool
}

func (s *postAcquireDeadlineStore) Acquire(_ context.Context, _ string, owner string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.clk.Advance(s.acquireElapsed)
	return persistence.LeaseToken{Version: 1, Owner: owner}, nil
}

func (*postAcquireDeadlineStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return token, nil
}

func (s *postAcquireDeadlineStore) Release(context.Context, string, persistence.LeaseToken) error {
	if s.connected != nil && s.connected.Load() {
		s.releasedWhileConnected.Store(true)
	}
	s.releases.Add(1)
	return nil
}

func (*postAcquireDeadlineStore) Current(context.Context, string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{}, nil
}

type postAcquireDeadlineSession struct {
	clk               *clocktest.Fake
	activationElapsed time.Duration
	events            chan ports.SessionEvent
	reconciled        chan struct{}
	reconcileOnce     sync.Once
	connected         atomic.Bool
	closes            atomic.Int32
}

func newPostAcquireDeadlineSession(clk *clocktest.Fake, elapsed time.Duration) *postAcquireDeadlineSession {
	return &postAcquireDeadlineSession{
		clk: clk, activationElapsed: elapsed,
		events: make(chan ports.SessionEvent), reconciled: make(chan struct{}),
	}
}

func (s *postAcquireDeadlineSession) Start(context.Context) error {
	s.connected.Store(true)
	return nil
}

func (s *postAcquireDeadlineSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	s.clk.Advance(s.activationElapsed)
	s.reconcileOnce.Do(func() { close(s.reconciled) })
	return nil
}

func (s *postAcquireDeadlineSession) Health(context.Context) ports.SessionHealth {
	connected := s.connected.Load()
	level := ports.ServiceLevelNone
	if connected {
		level = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: connected, Ready: connected, ServiceLevel: level}
}

func (s *postAcquireDeadlineSession) Events() <-chan ports.SessionEvent { return s.events }

func (s *postAcquireDeadlineSession) Close(context.Context) error {
	s.connected.Store(false)
	s.closes.Add(1)
	return nil
}

func TestSessionManager_PostAcquireActivationUsesFullLeaseSafeBudget(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC))
	store := &postAcquireDeadlineStore{clk: fake}
	// HA recurring reconnect reconciliation is capped at 3s, but initial
	// activation owns LeaseTTL(45s)-teardown(5s)=40s. Four seconds therefore
	// proves the short recurring-event cap is not reused here.
	sess := newPostAcquireDeadlineSession(fake, 4*time.Second)
	cfg := HAConfig("post-acquire-full-budget", true)
	cfg.ConnectAfterLease = true
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()
	wait.RequireReceive(t, sess.reconciled, 2*time.Second)
	wait.Silent(t, runErr, 25*time.Millisecond)
	if got := sess.closes.Load(); got != 0 {
		t.Fatalf("activation inside full lease-safe budget closed session: %d", got)
	}
	if _, held := mgr.Token(); !held {
		t.Fatal("activation inside full lease-safe budget lost local authorization")
	}
	cancel()
	if err := wait.RequireReceive(t, runErr, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v, want context.Canceled", err)
	}
}

func TestSessionManager_PostAcquireActivationAtSafeDeadlineBoundary(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC))
	boundaryClock := &postAcquireBoundaryClock{Fake: fake}
	store := &postAcquireDeadlineStore{clk: fake, acquireElapsed: 42 * time.Second}
	// TTL 45s with a 1s teardown/release margin: the local safe activation
	// deadline is t0+44s. Acquire consumed 42s, leaving exactly 2s of the
	// full lease-safe budget.
	sess := newPostAcquireDeadlineSession(fake, 2*time.Second)
	mgr := NewWithMetrics(Config{
		SessionID: "post-acquire-boundary", Exclusive: true, ConnectAfterLease: true,
		LeaseTTL: 45 * time.Second, RenewInterval: 10 * time.Second,
		RenewCallTimeout: 3 * time.Second, MaxRenewFails: 3, StepDownGrace: time.Second,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(boundaryClock))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()
	wait.RequireReceive(t, sess.reconciled, 2*time.Second)
	wait.Silent(t, runErr, 25*time.Millisecond)
	if got := store.releases.Load(); got != 0 {
		t.Fatalf("activation completing on the safe boundary released lease: %d", got)
	}
	if got := sess.closes.Load(); got != 0 {
		t.Fatalf("activation completing on the safe boundary closed session: %d", got)
	}
	if _, held := mgr.Token(); !held {
		t.Fatal("activation completing on the safe boundary lost local authorization")
	}
	cancel()
	if err := wait.RequireReceive(t, runErr, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v, want context.Canceled", err)
	}
}

func TestSessionManager_PostAcquireActivationOneNanosecondOverSafeDeadlineFailsTerminal(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC))
	boundaryClock := &postAcquireBoundaryClock{Fake: fake}
	store := &postAcquireDeadlineStore{clk: fake, acquireElapsed: 42 * time.Second}
	sess := newPostAcquireDeadlineSession(fake, 2*time.Second+time.Nanosecond)
	store.connected = &sess.connected
	mgr := NewWithMetrics(Config{
		SessionID: "post-acquire-over", Exclusive: true, ConnectAfterLease: true,
		LeaseTTL: 45 * time.Second, RenewInterval: 10 * time.Second,
		RenewCallTimeout: 3 * time.Second, MaxRenewFails: 3, StepDownGrace: time.Second,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(boundaryClock))

	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(context.Background()) }()
	wait.RequireReceive(t, sess.reconciled, 2*time.Second)
	err := wait.RequireReceive(t, runErr, 2*time.Second)
	if !errors.Is(err, ErrSessionUnrecoverable) {
		t.Fatalf("one-nanosecond-over error = %v, want ErrSessionUnrecoverable", err)
	}
	if got := sess.closes.Load(); got != 1 {
		t.Fatalf("one-nanosecond-over source closes = %d, want 1", got)
	}
	if got := store.releases.Load(); got != 1 {
		t.Fatalf("one-nanosecond-over lease releases = %d, want 1 after teardown", got)
	}
	if store.releasedWhileConnected.Load() {
		t.Fatal("one-nanosecond-over released the lease before disconnect")
	}
	if _, held := mgr.Token(); held {
		t.Fatal("one-nanosecond-over manager retained local authorization")
	}
}

var _ clock.Clock = (*postAcquireBoundaryClock)(nil)
var _ ports.LeaseStore = (*postAcquireDeadlineStore)(nil)
var _ ports.Session = (*postAcquireDeadlineSession)(nil)
