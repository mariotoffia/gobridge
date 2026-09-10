package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// stubLeaseStore is a minimal LeaseStore used for Locator clock tests.
// It only implements Current; other methods panic so a misuse is surfaced
// immediately.
type stubLeaseStore struct {
	mu       sync.Mutex
	info     persistence.LeaseInfo
	calls    int32
	err      error
	errUntil int // first N Current calls return err; 0 disables
}

func (s *stubLeaseStore) setInfo(info persistence.LeaseInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info = info
}

func (s *stubLeaseStore) setErrForNCalls(err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	s.errUntil = n
}

func (s *stubLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errUntil > 0 {
		s.errUntil--
		return persistence.LeaseInfo{}, s.err
	}
	info := s.info
	info.LeaseID = leaseID
	return info, nil
}

func (s *stubLeaseStore) Acquire(context.Context, string, string, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	panic("stubLeaseStore.Acquire not implemented")
}
func (s *stubLeaseStore) Renew(context.Context, string, persistence.LeaseToken, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	panic("stubLeaseStore.Renew not implemented")
}
func (s *stubLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	panic("stubLeaseStore.Release not implemented")
}

func (s *stubLeaseStore) callCount() int32 { return atomic.LoadInt32(&s.calls) }

// TestLocator_CacheTTL_FakeClock verifies that cached lease lookups
// are honored for CacheTTL and the next Locate after the TTL expires
// refetches from the underlying lease store. The test uses a fake clock
// so there are no wall-clock sleeps.
func TestLocator_CacheTTL_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &stubLeaseStore{}
	store.setInfo(persistence.LeaseInfo{
		Owner:   "instance-remote",
		Version: 1,
		// A live lease always carries a future ExpiresAt (both real stores set
		// it); required so the fresh-read expiry bound treats this
		// as a live row rather than an expired corpse.
		ExpiresAt: fake.Now().Add(time.Hour),
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})

	cfg := LocatorConfig{
		CacheTTL:       500 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 5 * time.Second,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")

	ctx := context.Background()

	peer, local, err := rl.Locate(ctx, "route-1")
	if err != nil {
		t.Fatalf("first Locate: %v", err)
	}
	if local {
		t.Fatal("expected remote owner (local=false) on first Locate")
	}
	if peer == nil || peer.InstanceID != "instance-remote" {
		t.Fatalf("unexpected peer: %+v", peer)
	}
	if got := store.callCount(); got != 1 {
		t.Fatalf("expected 1 lease fetch after first Locate, got %d", got)
	}

	fake.Advance(499 * time.Millisecond)
	if _, _, err := rl.Locate(ctx, "route-1"); err != nil {
		t.Fatalf("cached Locate: %v", err)
	}
	if got := store.callCount(); got != 1 {
		t.Fatalf("expected cached Locate to not refetch, got %d calls", got)
	}

	fake.Advance(2 * time.Millisecond)
	if _, _, err := rl.Locate(ctx, "route-1"); err != nil {
		t.Fatalf("post-TTL Locate: %v", err)
	}
	if got := store.callCount(); got != 2 {
		t.Fatalf("expected refetch after CacheTTL advance, got %d calls", got)
	}
}

// TestLocator_CircuitCooldown_FakeClock verifies that after MaxFailures
// consecutive Current() errors the circuit opens and Locate short-circuits
// (without hitting the lease store) until CooldownPeriod advances on the fake
// clock. With the default fail-CLOSED posture the open breaker
// surfaces an error and never processes locally, so a non-owner cannot act on
// an exclusive route during a store outage.
func TestLocator_CircuitCooldown_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &stubLeaseStore{}

	cfg := LocatorConfig{
		CacheTTL:       100 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 2 * time.Second,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")

	ctx := context.Background()
	boom := errors.New("lease store down")
	store.setErrForNCalls(boom, 100) // keep failing

	// Drive MaxFailures consecutive Current() errors without advancing
	// the clock — all failures stamp lastFailure at the same instant so
	// the cooldown window has a well-defined origin.
	for i := 0; i < cfg.MaxFailures; i++ {
		_, _, err := rl.Locate(ctx, "route-1")
		if err == nil {
			t.Fatalf("failure %d: expected error, got nil", i+1)
		}
	}
	if got := store.callCount(); int(got) != cfg.MaxFailures {
		t.Fatalf("expected %d lease store calls during failures, got %d", cfg.MaxFailures, got)
	}

	callsAtOpen := store.callCount()

	// Breaker open: fail-CLOSED default returns an error and does NOT touch the
	// store or process locally.
	peer, local, err := rl.Locate(ctx, "route-1")
	if err == nil {
		t.Fatal("open breaker must fail closed (return error) by default")
	}
	if local {
		t.Fatal("open breaker must NOT process locally under the fail-closed default")
	}
	if peer != nil {
		t.Fatalf("open breaker peer must be nil, got %+v", peer)
	}
	if got := store.callCount(); got != callsAtOpen {
		t.Fatalf("expected no lease store call while circuit open, got %d (was %d)",
			got, callsAtOpen)
	}

	fake.Advance(cfg.CooldownPeriod - time.Millisecond)
	if _, local, err = rl.Locate(ctx, "route-1"); err == nil || local {
		t.Fatal("circuit still open just before cooldown elapses; expected fail-closed short-circuit")
	}
	if got := store.callCount(); got != callsAtOpen {
		t.Fatalf("expected no lease store call while circuit still open, got %d", got)
	}

	store.setErrForNCalls(nil, 0)
	store.setInfo(persistence.LeaseInfo{Owner: "instance-local", ExpiresAt: fake.Now().Add(time.Hour)})

	fake.Advance(2 * time.Millisecond)
	_, local, err = rl.Locate(ctx, "route-1")
	if err != nil {
		t.Fatalf("post-cooldown Locate: %v", err)
	}
	if !local {
		t.Fatal("after cooldown, successful fetch to local owner should be local=true")
	}
	if got := store.callCount(); got != callsAtOpen+1 {
		t.Fatalf("expected one lease store call after cooldown, got %d (was %d)",
			got, callsAtOpen)
	}
}

// TestLocator_CircuitCooldown_FailOpen verifies the opt-in fail-OPEN posture
// (LocatorConfig.FailOpen = true): an open breaker processes locally
// (local=true, no error) instead of failing closed, trading exclusivity for
// availability.
func TestLocator_CircuitCooldown_FailOpen(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &stubLeaseStore{}
	cfg := LocatorConfig{
		CacheTTL:       100 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 2 * time.Second,
		FailOpen:       true,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")

	ctx := context.Background()
	store.setErrForNCalls(errors.New("lease store down"), 100)

	// With fail-open, the store errors still trip the breaker but each Locate
	// falls back to local processing (no error surfaced to the caller).
	for i := 0; i < cfg.MaxFailures; i++ {
		if _, local, _ := rl.Locate(ctx, "route-1"); !local {
			t.Fatalf("failure %d: fail-open store error must process locally", i+1)
		}
	}
	callsAtOpen := store.callCount()

	// Breaker open: fail-open short-circuits to local=true without touching the
	// store.
	peer, local, err := rl.Locate(ctx, "route-1")
	if err != nil {
		t.Fatalf("fail-open open breaker must not return error, got %v", err)
	}
	if !local {
		t.Fatal("fail-open open breaker must process locally")
	}
	if peer != nil {
		t.Fatalf("fail-open open breaker peer must be nil, got %+v", peer)
	}
	if got := store.callCount(); got != callsAtOpen {
		t.Fatalf("expected no lease store call while circuit open, got %d", got)
	}
}

// TestLocator_NotFoundDoesNotTripBreaker pins the primary fix: a
// not-found lease (the lease is momentarily unowned during an ordinary
// transfer) is NOT a store failure and must never advance the breaker toward
// opening, no matter how many transfers occur. Each such Locate fails closed
// (error, no local processing) by default, but the store is still consulted
// every time because the breaker stays shut.
func TestLocator_NotFoundDoesNotTripBreaker(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &stubLeaseStore{}
	cfg := LocatorConfig{
		CacheTTL:       100 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 2 * time.Second,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")

	ctx := context.Background()
	// Simulate an unowned lease on every call (many more than MaxFailures).
	store.setErrForNCalls(shared.ErrNotFound, 100)

	const attempts = 10 // >> MaxFailures
	for i := 0; i < attempts; i++ {
		peer, local, err := rl.Locate(ctx, "route-1")
		if !errors.Is(err, shared.ErrNoRouteOwner) {
			t.Fatalf("attempt %d: expected ErrNoRouteOwner, got %v", i+1, err)
		}
		if local || peer != nil {
			t.Fatalf("attempt %d: not-found must fail closed (no local, no peer)", i+1)
		}
	}
	// The breaker never opened: every attempt reached the store.
	if got := store.callCount(); int(got) != attempts {
		t.Fatalf("not-found must not trip the breaker; expected %d store calls, got %d",
			attempts, got)
	}
}
