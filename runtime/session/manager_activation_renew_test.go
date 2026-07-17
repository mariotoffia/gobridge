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

type activationRenewStore struct {
	renewErr                         error
	renewed                          chan struct{}
	renewOnce                        sync.Once
	acquires                         atomic.Int32
	renews                           atomic.Int32
	releases                         atomic.Int32
	connected                        *atomic.Bool
	activationReturned               *atomic.Bool
	releasedBeforeDisconnectOrReturn atomic.Bool
}

func (s *activationRenewStore) Acquire(ctx context.Context, _ string, owner string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	if s.acquires.Add(1) > 1 {
		<-ctx.Done()
		return persistence.LeaseToken{}, ctx.Err()
	}
	return persistence.LeaseToken{Version: 1, Owner: owner}, nil
}
func (s *activationRenewStore) Renew(context.Context, string, persistence.LeaseToken, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	s.renews.Add(1)
	s.renewOnce.Do(func() { close(s.renewed) })
	if s.renewErr != nil {
		return persistence.LeaseToken{}, s.renewErr
	}
	return persistence.LeaseToken{Version: 1, Owner: "owner-1"}, nil
}
func (s *activationRenewStore) Release(context.Context, string, persistence.LeaseToken) error {
	if (s.connected != nil && s.connected.Load()) ||
		(s.activationReturned != nil && !s.activationReturned.Load()) {
		s.releasedBeforeDisconnectOrReturn.Store(true)
	}
	s.releases.Add(1)
	return nil
}
func (*activationRenewStore) Current(context.Context, string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{}, shared.ErrNotFound
}

type renewalActivationSession struct {
	clk                 *clocktest.Fake
	renewed             <-chan struct{}
	waitForCancellation bool
	entered             chan struct{}
	reconciled          chan struct{}
	canceled            chan struct{}
	enterOnce           sync.Once
	reconcileOnce       sync.Once
	cancelOnce          sync.Once
	connected           atomic.Bool
	activationReturned  atomic.Bool
	closes              atomic.Int32
}

func newRenewalActivationSession(clk *clocktest.Fake, renewed <-chan struct{}, waitForCancellation bool) *renewalActivationSession {
	return &renewalActivationSession{
		clk: clk, renewed: renewed, waitForCancellation: waitForCancellation,
		entered: make(chan struct{}), reconciled: make(chan struct{}), canceled: make(chan struct{}),
	}
}
func (s *renewalActivationSession) Start(context.Context) error {
	s.connected.Store(true)
	return nil
}
func (s *renewalActivationSession) Reconcile(ctx context.Context, _ connectivity.SessionPlan) error {
	s.enterOnce.Do(func() { close(s.entered) })
	s.clk.Advance(time.Second)
	if s.waitForCancellation {
		<-ctx.Done()
		s.activationReturned.Store(true)
		s.cancelOnce.Do(func() { close(s.canceled) })
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.renewed:
	}
	s.clk.Advance(2 * time.Second)
	s.reconcileOnce.Do(func() { close(s.reconciled) })
	return nil
}
func (s *renewalActivationSession) Health(context.Context) ports.SessionHealth {
	connected := s.connected.Load()
	level := ports.ServiceLevelNone
	if connected {
		level = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: connected, Ready: connected, ServiceLevel: level}
}
func (*renewalActivationSession) Events() <-chan ports.SessionEvent {
	return make(chan ports.SessionEvent)
}
func (s *renewalActivationSession) Close(context.Context) error {
	s.connected.Store(false)
	s.closes.Add(1)
	return nil
}

func activationRenewConfig(id string) Config {
	return Config{
		SessionID: id, Exclusive: true, ConnectAfterLease: true,
		LeaseTTL: 5 * time.Second, RenewInterval: time.Second,
		RenewCallTimeout: 100 * time.Millisecond, MaxRenewFails: 1,
		StepDownGrace: time.Second, PostAcquireActivationTimeout: 10 * time.Second,
	}
}

func TestSessionManager_RenewsLeaseWhilePostAcquireActivationRuns(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))
	store := &activationRenewStore{renewed: make(chan struct{})}
	sess := newRenewalActivationSession(fake, store.renewed, false)
	mgr := NewWithMetrics(activationRenewConfig("activation-renews"), sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	wait.RequireReceive(t, sess.entered, time.Second)
	wait.RequireReceive(t, store.renewed, time.Second)
	wait.RequireReceive(t, sess.reconciled, time.Second)
	if got := store.renews.Load(); got < 1 {
		t.Fatalf("renew calls during activation = %d, want >=1", got)
	}
	if _, held := mgr.Token(); !held {
		t.Fatal("manager lost lease after renewed activation")
	}
	cancel()
	if err := wait.RequireReceive(t, runErr, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v, want context.Canceled", err)
	}
}

func TestSessionManager_LeaseLossMidActivationCancelsAndDisconnectsBeforeReturn(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))
	store := &activationRenewStore{
		renewed:  make(chan struct{}),
		renewErr: shared.ErrStaleFencingToken.WithMessage("lost during activation"),
	}
	sess := newRenewalActivationSession(fake, store.renewed, true)
	store.connected = &sess.connected
	store.activationReturned = &sess.activationReturned
	mgr := NewWithMetrics(activationRenewConfig("activation-lost"), sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))
	mgr.SetDrainIdleCheck(func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	wait.RequireReceive(t, sess.entered, time.Second)
	wait.RequireReceive(t, store.renewed, time.Second)
	wait.RequireReceive(t, sess.canceled, time.Second)
	wait.Until(t, time.Second, "source disconnect after activation lease loss", func() bool {
		return sess.closes.Load() > 0
	})
	if sess.connected.Load() {
		t.Fatal("source remained connected after lease loss during activation")
	}
	wait.Until(t, time.Second, "lease release after activation settled and source disconnected", func() bool {
		return store.releases.Load() > 0
	})
	if store.releasedBeforeDisconnectOrReturn.Load() {
		t.Fatal("lease released before activation callback returned and source disconnected")
	}
	wait.Silent(t, runErr, 25*time.Millisecond)
	cancel()
	if err := wait.RequireReceive(t, runErr, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after loss and cancellation = %v, want context.Canceled", err)
	}
}
