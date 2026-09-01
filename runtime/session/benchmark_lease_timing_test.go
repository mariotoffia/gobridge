package session

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// Baselines for the two paths whose cost changed with the lease-cadence
// validation and the close-before-release teardown order.

// BenchmarkConfigValidate measures one preflight validation. It now resolves the
// FULL cadence — derivation, jitter, per-call timeout, expiry-margin clamp and
// standby poll — instead of inspecting only an explicitly pinned RenewInterval,
// so it does the same work Manager construction does. It runs once per exclusive
// session per build/commit, which is why a config with only lease_ttl (the
// derived path, the most work) is measured alongside a fully pinned one.
func BenchmarkConfigValidate(b *testing.B) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{name: "derived_from_ttl", cfg: Config{SessionID: "bench", Exclusive: true, LeaseTTL: 45 * time.Second}},
		{name: "fully_pinned", cfg: HAConfig("bench", true)},
		{name: "default_preset", cfg: DefaultConfig("bench", true)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := tc.cfg.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkManagerClose measures the manager teardown that now closes the source
// through the goroutine-raced bounded call before releasing the lease. The race
// costs one goroutine and one injected-clock timer per Close; this pins that
// against the cost of the lease Release it guards. Close runs once per session
// per process shutdown, so the absolute numbers matter less than keeping the
// ordering guarantee from becoming a per-teardown allocation surprise.
func BenchmarkManagerClose(b *testing.B) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sess := &closeContextSession{events: make(chan ports.SessionEvent)}
	store := newLeaseLossStore(1<<30, nil)
	mgr := NewWithMetrics(Config{
		SessionID:     "bench-close",
		Exclusive:     true,
		LeaseTTL:      45 * time.Second,
		StepDownGrace: 5 * time.Second,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		mgr.mu.Lock()
		mgr.hasLease = true
		mgr.token = persistence.LeaseToken{Version: 1, Owner: "owner-1"}
		mgr.mu.Unlock()
		if err := mgr.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
