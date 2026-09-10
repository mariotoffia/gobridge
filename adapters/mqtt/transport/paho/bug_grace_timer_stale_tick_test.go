package paho

import (
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// ═══════════════════════════════════════════════════════════════════════════
// (LOW): the grace worker must not act on a STALE timer tick. beginGrace /
// rearmGrace re-arm the shared grace timer with Timer.Reset from another
// goroutine; if the timer had already fired, a tick is left queued in the timer
// channel and the worker would sweep immediately after the re-arm — BEFORE the
// re-armed deadline — ack-dropping orphans and firing retention metrics early.
// sweepIfExpired guards the sweep on the (always-advanced-on-arm) graceDeadline:
// a tick that arrives while now < graceDeadline is premature and must be
// ignored (and the timer re-armed for the remainder).
//
// This drives sweepIfExpired directly (the exact code the worker runs on a
// tick) so the assertion is deterministic and free of goroutine timing.
//
// Mutation killed:
//   - drop the `remaining > 0` guard (always sweep) → the premature-tick case
//     settles the orphan early: PendingCount drops to 0 and the ack fires, so
//     both "not swept early" assertions fail.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_GraceTimer_StaleTickDoesNotSweepEarly(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	grace := 30 * time.Second
	r := newRouter(nil, nil, withRouterClock(fake), withUnmatchedGrace(grace))
	// covered is nil, so any pending entry classifies as a true orphan a real
	// sweep would ack-and-drop — the observable signal a premature sweep leaks.

	var acked atomic.Bool
	r.mu.Lock()
	r.pending = append(r.pending, pendingPublish{
		pub:   &pahov5.Publish{Topic: "orphan/x"},
		ack:   func() error { acked.Store(true); return nil },
		epoch: r.connEpoch,
	})
	r.mu.Unlock()

	// newRouter seeded graceDeadline = now + grace, i.e. the window is still
	// open. A tick arriving now is stale and must be ignored.
	r.sweepIfExpired()
	require.Equal(t, 1, r.PendingCount(),
		"a stale tick within the grace window must NOT sweep the orphan")
	require.False(t, acked.Load(),
		"a stale tick must NOT ack the orphan early")

	// Once the window has genuinely elapsed, the same call settles the orphan.
	fake.Advance(grace + time.Second)
	r.sweepIfExpired()
	require.Equal(t, 0, r.PendingCount(),
		"after the deadline the orphan is settled")
	require.True(t, acked.Load(),
		"a real expiry acks and drops the orphan")
}
