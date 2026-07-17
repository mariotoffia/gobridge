package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// A-4 (MEDIUM): the session-takeover reconnect penalty must decay once the
// storm resolves, so an ordinary reconnect is not stuck paying a stale storm's
// backoff and busting the failover window.
//
// takeoverStreak is only reset when a NEW takeover arrives after a stable
// connection (noteSessionTakeover). If the collision RESOLVES (no more
// takeovers), nothing resets the streak — so before the recency gate, every
// later reconnect (a network blip, unrelated to any collision) kept paying the
// accumulated penalty (up to 64s) forever. takeoverPenalty now gates on the
// time since the LAST takeover: once takeoverStabilityWindow passes with none,
// the penalty is 0.
//
// Mutation killed: drop the `last == 0 || now-last >= window` recency clause in
// takeoverPenalty → after the window elapses the penalty stays at the storm's
// accumulated value and the second assertion (== 0) fails.
// ═══════════════════════════════════════════════════════════════════════════
func TestNoteSessionTakeover_ResolvedStorm_PenaltyDecaysWithoutNewTakeover(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "takeover-decay",
		Clock:      clk,
	}, connectivity.SessionExclusive, nil)

	// Connection just came up, then a storm of takeovers a few seconds apart
	// (well inside the stability window) accumulates a non-zero penalty.
	sess.mu.Lock()
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()
	for i := 0; i < 4; i++ {
		clk.Advance(2 * time.Second)
		sess.handleServerDisconnect(disconnectSessionTakenOver)
	}
	require.Greater(t, sess.takeoverPenalty(), time.Duration(0),
		"an active takeover storm carries a reconnect penalty")

	// The collision RESOLVES: no further takeovers arrive. Nothing has reset
	// the (still-high) streak, but once the stability window elapses with no
	// takeover the storm is over — the penalty must decay to 0. No takeover
	// occurs between the assertion above and this one, so the ONLY thing that
	// changed is elapsed time: this isolates the recency gate.
	clk.Advance(takeoverStabilityWindow)
	require.Equal(t, time.Duration(0), sess.takeoverPenalty(),
		"A-4: a resolved storm's penalty decays once no takeover has occurred for the stability window")
}
