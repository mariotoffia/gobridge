package memorylease_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Validates the in-memory lease store against the shared conformance suite with a fake clock.
func TestConformanceSuite(t *testing.T) {
	clk := clocktest.New()
	s := memorylease.NewStore(memorylease.WithClock(clk))

	storetest.RunLeaseStoreTests(t, s, &storetest.LeaseTestOptions{
		LeaseTTL: 10 * time.Second,
		WaitForExpiry: func(ttl time.Duration) {
			clk.Advance(ttl + time.Second)
		},
	})
}

// Verifies Acquire grants a new lease with a positive version and expected owner.
func TestAcquireFreshLease(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.Version == 0 {
		t.Fatal("version must be > 0")
	}
	if tok.Owner != "owner-A" {
		t.Fatalf("owner: got %q, want %q", tok.Owner, "owner-A")
	}
}

// Verifies Acquire returns ErrAlreadyExists when the lease is held by another owner.
func TestAcquireAlreadyHeldLease(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	_, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = s.Acquire(ctx, "lease-1", "owner-B", 30*time.Second, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

// Verifies a new owner can Acquire after the previous lease expires with an increased version.
func TestAcquireExpiredLease(t *testing.T) {
	now := time.Now()
	clk := clocktest.NewAt(now)
	s := memorylease.NewStore(memorylease.WithClock(clk))
	ctx := context.Background()

	tok1, err := s.Acquire(ctx, "lease-1", "owner-A", 10*time.Second, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Advance past expiry.
	clk.Advance(11 * time.Second)

	tok2, err := s.Acquire(ctx, "lease-1", "owner-B", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("takeover acquire: %v", err)
	}
	if tok2.Version <= tok1.Version {
		t.Fatalf("version must increase: v1=%d, v2=%d", tok1.Version, tok2.Version)
	}
	if tok2.Owner != "owner-B" {
		t.Fatalf("owner: got %q, want %q", tok2.Owner, "owner-B")
	}
}

// Verifies Renew extends expiry without changing the fencing version when the token matches.
func TestRenewSuccess(t *testing.T) {
	now := time.Now()
	clk := clocktest.NewAt(now)
	s := memorylease.NewStore(memorylease.WithClock(clk))
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 10*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	clk.Advance(5 * time.Second)
	renewed, err := s.Renew(ctx, "lease-1", tok, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Version != tok.Version {
		t.Fatalf("renew should keep same version: got %d, want %d", renewed.Version, tok.Version)
	}

	info, err := s.Current(ctx, "lease-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	expectedExpiry := now.Add(5*time.Second + 10*time.Second)
	if !info.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf("expiry: got %v, want %v", info.ExpiresAt, expectedExpiry)
	}
}

// Verifies Renew returns ErrStaleFencingToken when the lease has expired (bug exposure test).
func TestRenewExpiredLease(t *testing.T) {
	now := time.Now()
	clk := clocktest.NewAt(now)
	s := memorylease.NewStore(memorylease.WithClock(clk))
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 10*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Advance past expiry.
	clk.Advance(11 * time.Second)

	_, err = s.Renew(ctx, "lease-1", tok, 10*time.Second, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

// Verifies Renew returns ErrStaleFencingToken when the version does not match.
func TestRenewStaleToken(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	stale := domain.LeaseToken{Version: tok.Version + 999, Owner: "owner-A"}
	_, err = s.Renew(ctx, "lease-1", stale, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

// Verifies Renew returns ErrStaleFencingToken when the owner does not match the lease.
func TestRenewWrongOwner(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	wrong := domain.LeaseToken{Version: tok.Version, Owner: "owner-B"}
	_, err = s.Renew(ctx, "lease-1", wrong, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

// Verifies Renew returns ErrNotFound for an unknown lease ID.
func TestRenewNonExistent(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	_, err := s.Renew(ctx, "no-such-lease", domain.LeaseToken{Version: 1, Owner: "x"}, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies Release removes the lease so Current returns ErrNotFound.
func TestReleaseSuccess(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := s.Release(ctx, "lease-1", tok); err != nil {
		t.Fatalf("release: %v", err)
	}

	_, err = s.Current(ctx, "lease-1")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after release, got %v", err)
	}
}

// Verifies Release returns ErrStaleFencingToken when the token version is stale.
func TestReleaseStaleToken(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	stale := domain.LeaseToken{Version: tok.Version + 1, Owner: "owner-A"}
	err = s.Release(ctx, "lease-1", stale)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

// Verifies Release returns ErrNotFound for an unknown lease ID.
func TestReleaseNonExistent(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	err := s.Release(ctx, "no-such-lease", domain.LeaseToken{Version: 1, Owner: "x"})
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies Current returns lease metadata consistent with the active token.
func TestCurrentReturnsInfo(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	tok, err := s.Acquire(ctx, "lease-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	info, err := s.Current(ctx, "lease-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if info.LeaseID != "lease-1" {
		t.Fatalf("leaseID: got %q, want %q", info.LeaseID, "lease-1")
	}
	if info.Owner != "owner-A" {
		t.Fatalf("owner: got %q, want %q", info.Owner, "owner-A")
	}
	if info.Version != tok.Version {
		t.Fatalf("version: got %d, want %d", info.Version, tok.Version)
	}
	if info.ExpiresAt.IsZero() {
		t.Fatal("expiresAt should not be zero")
	}
}

// Verifies Current returns ErrNotFound when no lease exists.
func TestCurrentNonExistent(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	_, err := s.Current(ctx, "no-such-lease")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies exactly one goroutine wins a contested Acquire and the rest get ErrAlreadyExists.
func TestConcurrentAcquire(t *testing.T) {
	s := memorylease.NewStore()
	ctx := context.Background()

	const goroutines = 20
	var wins, losses atomic.Int32
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(owner string) {
			defer wg.Done()
			_, err := s.Acquire(ctx, "contested-lease", owner, 30*time.Second, nil)
			if err == nil {
				wins.Add(1)
			} else if errors.Is(err, shared.ErrAlreadyExists) {
				losses.Add(1)
			} else {
				t.Errorf("unexpected error from goroutine %s: %v", owner, err)
			}
		}(fmt.Sprintf("owner-%d", i))
	}
	wg.Wait()

	if w := wins.Load(); w != 1 {
		t.Fatalf("exactly one goroutine should win: got %d", w)
	}
	if l := losses.Load(); l != goroutines-1 {
		t.Fatalf("all others should lose: got %d", l)
	}
}

// Verifies fencing versions increase across repeated acquire-release cycles on the same lease.
func TestVersionMonotonicallyIncreases(t *testing.T) {
	now := time.Now()
	clk := clocktest.NewAt(now)
	s := memorylease.NewStore(memorylease.WithClock(clk))
	ctx := context.Background()

	var versions []uint64
	for i := 0; i < 5; i++ {
		tok, err := s.Acquire(ctx, "lease-cycling", fmt.Sprintf("owner-%d", i), 5*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire cycle %d: %v", i, err)
		}
		versions = append(versions, tok.Version)

		if err := s.Release(ctx, "lease-cycling", tok); err != nil {
			t.Fatalf("release cycle %d: %v", i, err)
		}
	}

	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("version did not increase: v[%d]=%d, v[%d]=%d", i-1, versions[i-1], i, versions[i])
		}
	}
}
