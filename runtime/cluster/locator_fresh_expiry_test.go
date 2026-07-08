package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Finding F3 (C3-M): the fresh-read path must apply the SAME ExpiresAt bound
// the cached stale-fallback path enforces. An expired-but-not-yet-seized lease
// row names a corpse owner; serving it as authoritative forwards exclusive
// traffic to a dead owner for up to TTL+observation. Past ExpiresAt the locator
// must fall through to the ownership-unknown posture (fail-closed) instead.
func TestLocator_FreshRead_BoundedByLeaseExpiry(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &stubLeaseStore{}
	// Fresh row is already expired at read time (ExpiresAt in the past) but still
	// names a remote owner: the classic expired-but-not-seized corpse.
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-remote",
		Version:   1,
		ExpiresAt: fake.Now().Add(-1 * time.Second),
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})

	cfg := LocatorConfig{
		CacheTTL:       1 * time.Second,
		MaxFailures:    100,
		CooldownPeriod: time.Hour,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")
	ctx := context.Background()

	// Fresh read (no cache primed): the expired row must NOT be served as the
	// authoritative owner; expect fail-closed ownership-unknown.
	peer, local, err := rl.Locate(ctx, "route-1")
	if err == nil {
		t.Fatal("an expired fresh lease row must NOT be served; expected fail-closed error")
	}
	if local || peer != nil {
		t.Fatalf("expired fresh read must not process locally or name a peer, got local=%v peer=%+v", local, peer)
	}

	// The corpse must not have been cached: a subsequent call re-reads the store
	// (rather than serving the expired owner from cache for a full CacheTTL).
	before := store.callCount()
	if _, _, err := rl.Locate(ctx, "route-1"); err == nil {
		t.Fatal("second call: expired row still must not be served")
	}
	if store.callCount() == before {
		t.Fatal("expired fresh row must not be cached; expected a re-read on the next call")
	}

	// Sanity: once the store returns a live (future-expiry) row, routing resumes.
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-remote",
		Version:   2,
		ExpiresAt: fake.Now().Add(10 * time.Second),
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})
	peer, local, err = rl.Locate(ctx, "route-1")
	if err != nil || local || peer == nil || peer.InstanceID != "instance-remote" {
		t.Fatalf("live fresh read should route to remote owner, got local=%v peer=%+v err=%v", local, peer, err)
	}
}

// TestLocator_FastCacheHit_BoundedByLeaseExpiry is the regression for finding
// F3-3a: the fast cache-hit path must apply the SAME ExpiresAt bound as the
// fresh-read and stale-fallback paths. A lease can expire INSIDE its CacheTTL
// window (nothing pins CacheTTL below lease_ttl), so a live owner cached and then
// dying while the cache entry is still "fresh" would be served as a corpse for
// the remainder of CacheTTL if the cache-hit path skipped the expiry check.
func TestLocator_FastCacheHit_BoundedByLeaseExpiry(t *testing.T) {
	// CacheTTL is deliberately far larger than the lease lifetime so the cache
	// entry stays "fresh" (within CacheTTL) well past its ExpiresAt — the exact
	// window the bound must close.
	newLocator := func(fake *clocktest.Fake, store *stubLeaseStore) *Locator {
		cfg := LocatorConfig{
			CacheTTL:       time.Hour,
			MaxFailures:    100,
			CooldownPeriod: time.Hour,
		}
		rl := NewLocator("instance-local", store, cfg, fake)
		rl.RegisterRoute("route-1", "sess-1")
		return rl
	}

	t.Run("corpse_within_cachettl_not_served", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		store := &stubLeaseStore{}
		// Prime the cache with a LIVE remote owner (expires 2s out).
		store.setInfo(persistence.LeaseInfo{
			Owner:     "instance-remote",
			Version:   1,
			ExpiresAt: fake.Now().Add(2 * time.Second),
			Endpoints: map[string]string{"http": "http://remote:8080"},
		})
		rl := newLocator(fake, store)
		ctx := context.Background()

		peer, local, err := rl.Locate(ctx, "route-1")
		if err != nil || local || peer == nil || peer.InstanceID != "instance-remote" {
			t.Fatalf("priming read should cache the live remote owner, got local=%v peer=%+v err=%v", local, peer, err)
		}
		primed := store.callCount()

		// Advance PAST the cached row's ExpiresAt but stay well within CacheTTL:
		// the entry is still "fresh" yet now names a corpse.
		fake.Advance(5 * time.Second)

		peer, local, err = rl.Locate(ctx, "route-1")
		if err == nil {
			t.Fatal("F3-3a: an expired cached owner must NOT be served from the fast cache-hit path; expected fail-closed")
		}
		if local || peer != nil {
			t.Fatalf("F3-3a: expired cache hit must not process locally or name a peer, got local=%v peer=%+v", local, peer)
		}
		if store.callCount() == primed {
			t.Fatal("F3-3a: expired cache hit must fall through to a fresh store read, but no re-read occurred")
		}
	})

	t.Run("exact_boundary_not_served", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		store := &stubLeaseStore{}
		store.setInfo(persistence.LeaseInfo{
			Owner:     "instance-remote",
			Version:   1,
			ExpiresAt: fake.Now().Add(2 * time.Second),
			Endpoints: map[string]string{"http": "http://remote:8080"},
		})
		rl := newLocator(fake, store)
		ctx := context.Background()

		if _, _, err := rl.Locate(ctx, "route-1"); err != nil {
			t.Fatalf("priming read failed: %v", err)
		}
		primed := store.callCount()

		// Advance to EXACTLY ExpiresAt: now.Before(ExpiresAt) is strict, so the
		// entry is expired at its own boundary (mirrors the fresh-read bound).
		fake.Advance(2 * time.Second)

		if _, _, err := rl.Locate(ctx, "route-1"); err == nil {
			t.Fatal("F3-3a: a cached owner exactly at its ExpiresAt must NOT be served; expected fail-closed")
		}
		if store.callCount() == primed {
			t.Fatal("F3-3a: exact-boundary cache hit must fall through to a fresh store read")
		}
	})
}
