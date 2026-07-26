package session

import (
	"context"
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

// minimalLeaseStore is a tiny in-memory LeaseStore used by the session
// manager clock test. It supports observing Renew calls via a channel so
// the test can synchronize with the renewal goroutine without wall-clock
// polling.
type minimalLeaseStore struct {
	mu      sync.Mutex
	version uint64
	owner   string
	onRenew chan struct{}
	renews  int32
}

func newMinimalLeaseStore(onRenew chan struct{}) *minimalLeaseStore {
	return &minimalLeaseStore{onRenew: onRenew}
}

func (s *minimalLeaseStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.owner = ownerID
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (s *minimalLeaseStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	atomic.AddInt32(&s.renews, 1)
	s.mu.Lock()
	owner := s.owner
	s.mu.Unlock()
	if s.onRenew != nil {
		s.onRenew <- struct{}{}
	}
	return persistence.LeaseToken{Version: token.Version, Owner: owner}, nil
}

func (s *minimalLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	return nil
}

func (s *minimalLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: s.owner, Version: s.version}, nil
}

func (s *minimalLeaseStore) renewCount() int32 { return atomic.LoadInt32(&s.renews) }

// stubSessionForClockTest is a minimal ports.Session whose lifecycle
// methods are no-ops and whose events channel never emits. It exists so
// the session manager's renew loop runs without interference from
// reconcile or event handling.
type stubSessionForClockTest struct {
	events chan ports.SessionEvent
}

func newStubSessionForClockTest() *stubSessionForClockTest {
	return &stubSessionForClockTest{events: make(chan ports.SessionEvent)}
}

func (s *stubSessionForClockTest) Start(context.Context) error { return nil }
func (s *stubSessionForClockTest) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}
func (s *stubSessionForClockTest) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *stubSessionForClockTest) Events() <-chan ports.SessionEvent { return s.events }
func (s *stubSessionForClockTest) Close(context.Context) error {
	select {
	case <-s.events:
	default:
		close(s.events)
	}
	return nil
}

// TestSessionManager_RenewInterval_FakeClock verifies that lease
// renewals fire at the configured RenewInterval on the injected Clock,
// not on wall-clock time. After the first lease acquisition the renew
// loop registers a timer; each Advance of RenewInterval on the fake
// clock must trigger exactly one leaseStore.Renew call.
func TestSessionManager_RenewInterval_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	renewCh := make(chan struct{}, 8)
	store := newMinimalLeaseStore(renewCh)
	session := newStubSessionForClockTest()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:     "sess-clock",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 10,
		StepDownGrace: 100 * time.Millisecond,
	}

	mgr := NewWithMetrics(cfg, session, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

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

	// Wait until the session manager has acquired the lease and the renew
	// goroutine has registered its interval timer with the fake clock
	// (TimerCount counts non-stopped, non-fired timers). The timer's channel
	// is buffered, so once it is registered an Advance can never lose the
	// fire even if the goroutine has not yet reached `<-timer.C()`.
	wait.Until(t, 2*time.Second, "lease acquired", func() bool { _, ok := mgr.Token(); return ok })
	wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

	if got := store.renewCount(); got != 0 {
		t.Fatalf("no renew should have fired yet, got %d", got)
	}

	// Without advancing the clock, the renew must NOT fire.
	select {
	case <-renewCh:
		t.Fatal("renew fired without clock advance")
	case <-time.After(30 * time.Millisecond):
	}

	// ── First renewal: advance by RenewInterval ───────────────────────
	fake.Advance(renewInterval)

	select {
	case <-renewCh:
	case <-time.After(2 * time.Second):
		t.Fatal("renew did not fire after RenewInterval advance")
	}
	if got := store.renewCount(); got != 1 {
		t.Fatalf("after first advance: expected 1 renew, got %d", got)
	}

	// Firing removed the timer from the fake clock (TimerCount drops to 0);
	// wait for the renew goroutine's timer.Reset to re-register it so the
	// next Advance is guaranteed to hit an armed timer.
	wait.Until(t, 2*time.Second, "renew timer re-armed", func() bool { return fake.TimerCount() >= 1 })

	// Still no second renew without further advance.
	select {
	case <-renewCh:
		t.Fatal("second renew fired without further clock advance")
	case <-time.After(30 * time.Millisecond):
	}

	// ── Second renewal: advance by RenewInterval again ────────────────
	fake.Advance(renewInterval)

	select {
	case <-renewCh:
	case <-time.After(2 * time.Second):
		t.Fatal("second renew did not fire after advance")
	}
	if got := store.renewCount(); got != 2 {
		t.Fatalf("after second advance: expected 2 renews, got %d", got)
	}
}
