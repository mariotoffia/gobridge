package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Finding C3-M: on a transient store error the locator may serve the last-known
// owner rather than fail, so a brief blip does not disrupt routing. But that
// stale fallback MUST be bounded by the lease's own expiry: a store outage can
// outlast the lease, after which the cached owner may have stepped down and a
// new owner (or none) taken over. Serving an age-unbounded stale owner would
// forward exclusive traffic to a non-owner indefinitely. Past ExpiresAt the
// locator must fall through to the ownership-unknown posture instead.
func TestLocator_StaleFallback_BoundedByLeaseExpiry(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &stubLeaseStore{}
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-remote",
		Version:   1,
		ExpiresAt: fake.Now().Add(10 * time.Second),
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})

	cfg := LocatorConfig{
		CacheTTL:       1 * time.Second,
		MaxFailures:    100, // keep the breaker shut across these few failures
		CooldownPeriod: time.Hour,
	}
	rl := NewLocator("instance-local", store, cfg, fake)
	rl.RegisterRoute("route-1", "sess-1")
	ctx := context.Background()

	// Prime the cache with a successful fetch (owner known, ExpiresAt = T+10s).
	if _, local, err := rl.Locate(ctx, "route-1"); err != nil || local {
		t.Fatalf("prime Locate: local=%v err=%v", local, err)
	}

	// Store now fails on every call.
	store.setErrForNCalls(errors.New("lease store down"), 100)

	// Blip while the lease is still valid (past CacheTTL, before ExpiresAt):
	// serve the cached owner rather than fail.
	fake.Advance(2 * time.Second)
	peer, local, err := rl.Locate(ctx, "route-1")
	if err != nil {
		t.Fatalf("in-window stale fallback should serve cached owner, got err=%v", err)
	}
	if local || peer == nil || peer.InstanceID != "instance-remote" {
		t.Fatalf("expected cached remote owner, got local=%v peer=%+v", local, peer)
	}

	// Outage outlasts the lease (past ExpiresAt): the stale owner may be wrong,
	// so fall through to fail-closed ownership-unknown instead of serving it.
	fake.Advance(9 * time.Second) // now T+11s > ExpiresAt (T+10s)
	peer, local, err = rl.Locate(ctx, "route-1")
	if err == nil {
		t.Fatal("past lease expiry the stale owner must NOT be served; expected fail-closed error")
	}
	if local || peer != nil {
		t.Fatalf("expired stale fallback must not process locally or name a peer, got local=%v peer=%+v", local, peer)
	}
}
