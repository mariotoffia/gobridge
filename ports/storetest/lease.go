package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// LeaseTestOptions configures the lease conformance test suite.
type LeaseTestOptions struct {
	// WaitForExpiry is called when the test needs to wait for a lease to expire.
	// The ttl parameter is the TTL used when the lease was acquired.
	// If nil, the suite uses time.Sleep(ttl + 500ms).
	WaitForExpiry func(ttl time.Duration)

	// LeaseTTL is the TTL for leases in expiry-related tests.
	// If zero, defaults to 2*time.Second.
	LeaseTTL time.Duration
}

func (o *LeaseTestOptions) waitForExpiry(ttl time.Duration) {
	if o != nil && o.WaitForExpiry != nil {
		o.WaitForExpiry(ttl)
		return
	}
	time.Sleep(ttl + 500*time.Millisecond) // ESSENTIAL: wall-clock fallback when the store offers no fake-clock hook — real emulator TTLs only expire in real time
}

func (o *LeaseTestOptions) leaseTTL() time.Duration {
	if o != nil && o.LeaseTTL > 0 {
		return o.LeaseTTL
	}
	return 2 * time.Second
}

// RunLeaseStoreTests executes the full conformance suite against the given
// LeaseStore. The caller is responsible for creating and closing the store.
func RunLeaseStoreTests(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	t.Helper()

	t.Run("AcquireFresh", func(t *testing.T) {
		leaseAcquireFresh(t, store)
	})
	t.Run("AcquireAlreadyHeld", func(t *testing.T) {
		leaseAcquireAlreadyHeld(t, store)
	})
	t.Run("AcquireExpiredTakeover", func(t *testing.T) {
		leaseAcquireExpiredTakeover(t, store, opts)
	})
	t.Run("TakeoverOnlyAfterTTL", func(t *testing.T) {
		leaseTakeoverOnlyAfterTTL(t, store, opts)
	})
	t.Run("RenewAfterTakeover", func(t *testing.T) {
		leaseRenewAfterTakeover(t, store, opts)
	})
	t.Run("RenewSuccess", func(t *testing.T) {
		leaseRenewSuccess(t, store)
	})
	t.Run("RenewStaleToken", func(t *testing.T) {
		leaseRenewStaleToken(t, store)
	})
	t.Run("RenewWrongOwner", func(t *testing.T) {
		leaseRenewWrongOwner(t, store)
	})
	t.Run("RenewNonExistent", func(t *testing.T) {
		leaseRenewNonExistent(t, store)
	})
	t.Run("RenewExpiredLease", func(t *testing.T) {
		leaseRenewExpiredLease(t, store, opts)
	})
	t.Run("ConcurrentRenew", func(t *testing.T) {
		leaseConcurrentRenew(t, store)
	})
	t.Run("ReleaseSuccess", func(t *testing.T) {
		leaseReleaseSuccess(t, store)
	})
	t.Run("ReleaseStaleToken", func(t *testing.T) {
		leaseReleaseStaleToken(t, store)
	})
	t.Run("ReleaseNonExistent", func(t *testing.T) {
		leaseReleaseNonExistent(t, store)
	})
	t.Run("ReleaseIdempotent", func(t *testing.T) {
		leaseReleaseIdempotent(t, store)
	})
	t.Run("CurrentReturnsInfo", func(t *testing.T) {
		leaseCurrentReturnsInfo(t, store)
	})
	t.Run("EndpointsIsolation", func(t *testing.T) {
		leaseEndpointsIsolation(t, store)
	})
	t.Run("CurrentNonExistent", func(t *testing.T) {
		leaseCurrentNonExistent(t, store)
	})
	t.Run("VersionMonotonicallyIncreases", func(t *testing.T) {
		leaseVersionMonotonicallyIncreases(t, store)
	})
	t.Run("ConcurrentAcquire", func(t *testing.T) {
		leaseConcurrentAcquire(t, store)
	})
	t.Run("FullLifecycle", func(t *testing.T) {
		leaseFullLifecycle(t, store, opts)
	})
}

func leaseAcquireFresh(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-af-1", "owner-A", 30*time.Second, nil)
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

func leaseAcquireAlreadyHeld(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	_, err := store.Acquire(ctx, "lt-ah-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = store.Acquire(ctx, "lt-ah-1", "owner-B", 30*time.Second, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func leaseAcquireExpiredTakeover(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	ctx := context.Background()
	ttl := opts.leaseTTL()

	tok1, err := store.Acquire(ctx, "lt-aet-1", "owner-A", ttl, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Pre-expiry takeover attempt. It MUST be rejected (the lease is still
	// held), and for a store that takes over via a local-clock observation
	// window (dynamodblease) it also establishes owner-B's
	// observation baseline so the post-expiry seize is permitted. A single-clock
	// store (memorylease) simply rejects it as still-held.
	if _, err := store.Acquire(ctx, "lt-aet-1", "owner-B", 30*time.Second, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("pre-expiry takeover must be rejected with ErrAlreadyExists, got %v", err)
	}

	opts.waitForExpiry(ttl)

	tok2, err := store.Acquire(ctx, "lt-aet-1", "owner-B", 30*time.Second, nil)
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

// leaseTakeoverOnlyAfterTTL pins that an actively-held lease cannot be seized
// until it has been observed unowned for a full TTL on the TAKER's own clock
// repeated takeover attempts before the TTL elapses must all be
// rejected, and only after the expiry wait does the takeover succeed.
func leaseTakeoverOnlyAfterTTL(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	ctx := context.Background()
	ttl := opts.leaseTTL()

	if _, err := store.Acquire(ctx, "lt-tat-1", "owner-A", ttl, nil); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Several rapid pre-expiry attempts: none may seize the lease.
	for i := 0; i < 3; i++ {
		if _, err := store.Acquire(ctx, "lt-tat-1", "owner-B", 30*time.Second, nil); !errors.Is(err, shared.ErrAlreadyExists) {
			t.Fatalf("attempt %d before TTL must be rejected with ErrAlreadyExists, got %v", i+1, err)
		}
	}

	opts.waitForExpiry(ttl)

	tok, err := store.Acquire(ctx, "lt-tat-1", "owner-B", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("takeover after TTL: %v", err)
	}
	if tok.Owner != "owner-B" {
		t.Fatalf("owner: got %q, want %q", tok.Owner, "owner-B")
	}
}

// leaseRenewAfterTakeover pins the definitive lease-loss signal the session
// layer relies on for immediate step-down: once a new owner has
// taken over an expired lease, the OLD owner's renew with its now-stale token
// must be fenced with ErrStaleFencingToken (never a transient error), while the
// new owner can still renew.
func leaseRenewAfterTakeover(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	ctx := context.Background()
	ttl := opts.leaseTTL()

	tokA, err := store.Acquire(ctx, "lt-rat-1", "owner-A", ttl, nil)
	if err != nil {
		t.Fatalf("owner-A acquire: %v", err)
	}

	// Establish owner-B's observation baseline for the observation-window store.
	if _, err := store.Acquire(ctx, "lt-rat-1", "owner-B", 30*time.Second, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("pre-expiry takeover must be rejected, got %v", err)
	}

	opts.waitForExpiry(ttl)

	tokB, err := store.Acquire(ctx, "lt-rat-1", "owner-B", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("owner-B takeover: %v", err)
	}

	// Old owner's renew with the stale token is definitively fenced.
	if _, err := store.Renew(ctx, "lt-rat-1", tokA, ttl, nil); !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale owner renew after takeover must return ErrStaleFencingToken, got %v", err)
	}

	// New owner can still renew.
	if _, err := store.Renew(ctx, "lt-rat-1", tokB, 30*time.Second, nil); err != nil {
		t.Fatalf("new owner renew after takeover: %v", err)
	}
}

// leaseReleaseIdempotent pins that releasing an already-released lease is a safe
// operation: it returns a DEFINITIVE signal (ErrNotFound or ErrStaleFencingToken,
// never a transient/opaque error), does not corrupt state, and leaves the lease
// re-acquirable with a strictly higher fencing version. The manager's
// release-then-reacquire recovery tolerates a double release, so
// the store must not surprise it.
func leaseReleaseIdempotent(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()

	tok, err := store.Acquire(ctx, "lt-ri-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.Release(ctx, "lt-ri-1", tok); err != nil {
		t.Fatalf("first release: %v", err)
	}

	// Second release of the same, now-released token must be safely definitive.
	err = store.Release(ctx, "lt-ri-1", tok)
	if !errors.Is(err, shared.ErrNotFound) && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("second release must return a definitive signal (ErrNotFound/ErrStaleFencingToken), got %v", err)
	}

	// The lease remains usable and monotonic after the double release.
	tok2, err := store.Acquire(ctx, "lt-ri-1", "owner-B", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("re-acquire after double release: %v", err)
	}
	if tok2.Version <= tok.Version {
		t.Fatalf("re-acquire version must increase: v1=%d, v2=%d", tok.Version, tok2.Version)
	}
}

func leaseRenewSuccess(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-rs-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	renewed, err := store.Renew(ctx, "lt-rs-1", tok, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Version != tok.Version {
		t.Fatalf("renew should keep same version: got %d, want %d", renewed.Version, tok.Version)
	}
	if renewed.Owner != tok.Owner {
		t.Fatalf("renew should keep same owner: got %q, want %q", renewed.Owner, tok.Owner)
	}
}

func leaseRenewStaleToken(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-rst-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	stale := persistence.LeaseToken{Version: tok.Version + 999, Owner: "owner-A"}
	_, err = store.Renew(ctx, "lt-rst-1", stale, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func leaseRenewWrongOwner(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-rwo-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	wrong := persistence.LeaseToken{Version: tok.Version, Owner: "owner-B"}
	_, err = store.Renew(ctx, "lt-rwo-1", wrong, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func leaseRenewNonExistent(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	_, err := store.Renew(ctx, "lt-rne-no-such-lease", persistence.LeaseToken{Version: 1, Owner: "x"}, 30*time.Second, nil)
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func leaseRenewExpiredLease(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	ctx := context.Background()
	ttl := opts.leaseTTL()

	tok, err := store.Acquire(ctx, "lt-rel-1", "owner-A", ttl, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	opts.waitForExpiry(ttl)

	_, err = store.Renew(ctx, "lt-rel-1", tok, ttl, nil)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func leaseReleaseSuccess(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-rels-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := store.Release(ctx, "lt-rels-1", tok); err != nil {
		t.Fatalf("release: %v", err)
	}

	_, err = store.Current(ctx, "lt-rels-1")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after release, got %v", err)
	}
}

func leaseReleaseStaleToken(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-relst-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	stale := persistence.LeaseToken{Version: tok.Version + 1, Owner: "owner-A"}
	err = store.Release(ctx, "lt-relst-1", stale)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func leaseReleaseNonExistent(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	err := store.Release(ctx, "lt-relne-no-such-lease", persistence.LeaseToken{Version: 1, Owner: "x"})
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func leaseCurrentReturnsInfo(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-cri-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	info, err := store.Current(ctx, "lt-cri-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if info.LeaseID != "lt-cri-1" {
		t.Fatalf("leaseID: got %q, want %q", info.LeaseID, "lt-cri-1")
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

func leaseCurrentNonExistent(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	_, err := store.Current(ctx, "lt-cne-no-such-lease")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func leaseVersionMonotonicallyIncreases(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()

	var versions []uint64
	for i := 0; i < 5; i++ {
		tok, err := store.Acquire(ctx, "lt-vmi-1", fmt.Sprintf("owner-%d", i), 30*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire cycle %d: %v", i, err)
		}
		versions = append(versions, tok.Version)

		if err := store.Release(ctx, "lt-vmi-1", tok); err != nil {
			t.Fatalf("release cycle %d: %v", i, err)
		}
	}

	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("version did not increase: v[%d]=%d, v[%d]=%d", i-1, versions[i-1], i, versions[i])
		}
	}
}

func leaseConcurrentAcquire(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()

	const goroutines = 20
	var wins, losses atomic.Int32
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(owner string) {
			defer wg.Done()
			_, err := store.Acquire(ctx, "lt-ca-contested", owner, 30*time.Second, nil)
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

func leaseConcurrentRenew(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	tok, err := store.Acquire(ctx, "lt-cr-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	const goroutines = 10
	var successes, failures atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := store.Renew(ctx, "lt-cr-1", tok, 30*time.Second, nil)
			if err == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if s := successes.Load(); s == 0 {
		t.Fatal("at least one renewal should succeed")
	}
}

func leaseFullLifecycle(t *testing.T, store ports.LeaseStore, opts *LeaseTestOptions) {
	ctx := context.Background()

	tok1, err := store.Acquire(ctx, "lt-fl-1", "owner-A", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	renewed, err := store.Renew(ctx, "lt-fl-1", tok1, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Version != tok1.Version {
		t.Fatalf("renew changed version: got %d, want %d", renewed.Version, tok1.Version)
	}

	info, err := store.Current(ctx, "lt-fl-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if info.Owner != "owner-A" || info.Version != tok1.Version {
		t.Fatalf("current mismatch: owner=%q version=%d", info.Owner, info.Version)
	}

	if err := store.Release(ctx, "lt-fl-1", tok1); err != nil {
		t.Fatalf("release: %v", err)
	}

	_, err = store.Current(ctx, "lt-fl-1")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after release, got %v", err)
	}

	tok2, err := store.Acquire(ctx, "lt-fl-1", "owner-B", 30*time.Second, nil)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if tok2.Version <= tok1.Version {
		t.Fatalf("re-acquire version must increase: v1=%d, v2=%d", tok1.Version, tok2.Version)
	}
}

// leaseEndpointsIsolation proves LeaseInfo is a read-only snapshot DTO:
// the LeaseStore defensively copies endpoints at every boundary, so
// neither mutating the caller's map after Acquire nor mutating a
// returned LeaseInfo.Endpoints can corrupt the store's view (finding
// #29).
func leaseEndpointsIsolation(t *testing.T, store ports.LeaseStore) {
	ctx := context.Background()
	const want = "10.0.0.1:50051"
	endpoints := map[string]string{"grpc": want}

	if _, err := store.Acquire(ctx, "lt-ep-iso", "owner-A", 30*time.Second, endpoints); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Mutating the caller's map after Acquire must not change the store:
	// the store must copy endpoints on the way in.
	endpoints["grpc"] = "tampered-in"
	endpoints["added"] = "late"

	info, err := store.Current(ctx, "lt-ep-iso")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if info.Endpoints["grpc"] != want {
		t.Fatalf("stored endpoint mutated via caller map: got %q, want %q", info.Endpoints["grpc"], want)
	}
	if _, ok := info.Endpoints["added"]; ok {
		t.Fatal("late-added caller endpoint leaked into the store")
	}

	// Mutating a returned LeaseInfo.Endpoints must not change a subsequent
	// Current(): the store must copy endpoints on the way out too.
	info.Endpoints["grpc"] = "tampered-out"
	info2, err := store.Current(ctx, "lt-ep-iso")
	if err != nil {
		t.Fatalf("current after return mutation: %v", err)
	}
	if info2.Endpoints["grpc"] != want {
		t.Fatalf("returned LeaseInfo.Endpoints aliases store state: got %q, want %q", info2.Endpoints["grpc"], want)
	}
}
