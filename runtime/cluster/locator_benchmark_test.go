package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// Locate runs per routing decision on exclusive routes, and the
// ownership-unknown disclosure adds a tagged counter emission to the path that
// already failed to name an owner. These two benchmarks bracket that cost: the
// served path (no emission) against the unknown path (one emission), so a
// regression in the disclosure shows up as a gap between them rather than as a
// number nobody can compare.

func benchLocator(b *testing.B, expiresIn time.Duration) *Locator {
	b.Helper()
	store := &stubLeaseStore{}
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-local",
		Version:   1,
		ExpiresAt: time.Now().Add(expiresIn),
	})
	rl := NewLocator("instance-local", store, LocatorConfig{
		CacheTTL:       time.Minute,
		MaxFailures:    1_000_000, // never open the breaker during the run
		CooldownPeriod: time.Hour,
		Metrics:        &ports.NoopExporter{},
	}, nil)
	rl.RegisterRoute("route-bench", "sess-bench")
	return rl
}

// BenchmarkLocator_Locate_OwnedLocal is the served path: a live, cached owner,
// no disclosure.
func BenchmarkLocator_Locate_OwnedLocal(b *testing.B) {
	rl := benchLocator(b, time.Hour)
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		_, _, _ = rl.Locate(ctx, "route-bench")
	}
}

// BenchmarkLocator_Locate_OwnerUnknown is the expired-owner path: a fresh store
// read every call (an expired row is never cached) plus the reason-tagged
// disclosure.
func BenchmarkLocator_Locate_OwnerUnknown(b *testing.B) {
	rl := benchLocator(b, -time.Hour)
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		_, _, _ = rl.Locate(ctx, "route-bench")
	}
}
