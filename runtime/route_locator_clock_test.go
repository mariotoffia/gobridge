package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// stubLeaseStore is a minimal LeaseStore used for routeLocator clock tests.
// It only implements Current; other methods panic so a misuse is surfaced
// immediately.
type stubLeaseStore struct {
	mu       sync.Mutex
	info     domain.LeaseInfo
	calls    int32
	err      error
	errUntil int // first N Current calls return err; 0 disables
}

func (s *stubLeaseStore) setInfo(info domain.LeaseInfo) {
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

func (s *stubLeaseStore) Current(_ context.Context, leaseID string) (domain.LeaseInfo, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errUntil > 0 {
		s.errUntil--
		return domain.LeaseInfo{}, s.err
	}
	info := s.info
	info.LeaseID = leaseID
	return info, nil
}

func (s *stubLeaseStore) Acquire(context.Context, string, string, time.Duration, map[string]string) (domain.LeaseToken, error) {
	panic("stubLeaseStore.Acquire not implemented")
}
func (s *stubLeaseStore) Renew(context.Context, string, domain.LeaseToken, time.Duration, map[string]string) (domain.LeaseToken, error) {
	panic("stubLeaseStore.Renew not implemented")
}
func (s *stubLeaseStore) Release(context.Context, string, domain.LeaseToken) error {
	panic("stubLeaseStore.Release not implemented")
}

func (s *stubLeaseStore) callCount() int32 { return atomic.LoadInt32(&s.calls) }

// TestRouteLocator_CacheTTL_FakeClock verifies that cached lease lookups
// are honored for CacheTTL and the next Locate after the TTL expires
// refetches from the underlying lease store. The test uses a fake clock
// so there are no wall-clock sleeps.
func TestRouteLocator_CacheTTL_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &stubLeaseStore{}
	store.setInfo(domain.LeaseInfo{
		Owner:     "instance-remote",
		Version:   1,
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})

	cfg := RouteLocatorConfig{
		CacheTTL:       500 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 5 * time.Second,
	}
	rl := newRouteLocator("instance-local", store, cfg, fake)
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

// TestRouteLocator_CircuitCooldown_FakeClock verifies that after
// MaxFailures consecutive Current() errors the circuit opens and Locate
// short-circuits (returning local=true without hitting the lease store)
// until CooldownPeriod advances on the fake clock.
func TestRouteLocator_CircuitCooldown_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &stubLeaseStore{}

	cfg := RouteLocatorConfig{
		CacheTTL:       100 * time.Millisecond,
		MaxFailures:    3,
		CooldownPeriod: 2 * time.Second,
	}
	rl := newRouteLocator("instance-local", store, cfg, fake)
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

	peer, local, err := rl.Locate(ctx, "route-1")
	if err != nil {
		t.Fatalf("during cooldown Locate should not return error, got %v", err)
	}
	if !local {
		t.Fatal("during cooldown Locate must short-circuit as local=true")
	}
	if peer != nil {
		t.Fatalf("during cooldown peer must be nil, got %+v", peer)
	}
	if got := store.callCount(); got != callsAtOpen {
		t.Fatalf("expected no lease store call while circuit open, got %d (was %d)",
			got, callsAtOpen)
	}

	fake.Advance(cfg.CooldownPeriod - time.Millisecond)
	if _, local, _ = rl.Locate(ctx, "route-1"); !local {
		t.Fatal("circuit still open just before cooldown elapses; expected short-circuit")
	}
	if got := store.callCount(); got != callsAtOpen {
		t.Fatalf("expected no lease store call while circuit still open, got %d", got)
	}

	store.setErrForNCalls(nil, 0)
	store.setInfo(domain.LeaseInfo{Owner: "instance-local"})

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
