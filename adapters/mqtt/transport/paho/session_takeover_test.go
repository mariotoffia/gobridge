package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Session-level production hardening: session_expiry_interval defaulting
// for non-ephemeral modes and ClientID-collision (session takeover)
// storm damping.
// ═══════════════════════════════════════════════════════════════════════════

// TestNewSession_ExpiryDefaultedForPersistentModes verifies that
// Persistent/Exclusive sessions with expiry 0 (zero offline retention —
// contradicting the mode's purpose) get DefaultPersistentSessionExpiry,
// while Ephemeral keeps 0 and an explicit value is preserved.
func TestNewSession_ExpiryDefaultedForPersistentModes(t *testing.T) {
	base := SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "expiry-test",
	}

	cases := []struct {
		name   string
		mode   connectivity.SessionMode
		expiry uint32
		want   uint32
	}{
		{"persistent zero → default", connectivity.SessionPersistent, 0, DefaultPersistentSessionExpiry},
		{"exclusive zero → default", connectivity.SessionExclusive, 0, DefaultPersistentSessionExpiry},
		{"ephemeral zero stays zero", connectivity.SessionEphemeral, 0, 0},
		{"persistent explicit preserved", connectivity.SessionPersistent, 3600, 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.SessionExpiryInterval = tc.expiry
			sess := NewSession(opts, tc.mode, nil)
			require.Equal(t, tc.want, sess.opts.SessionExpiryInterval)
		})
	}
}

// TestNoteSessionTakeover_DampsCollisionStorm verifies the escalation
// contract for repeated 0x8E disconnects without intervening stability:
// no penalty for the first takeover (legitimate failover), exponential
// penalty afterwards (capped), metric counted per occurrence.
func TestNoteSessionTakeover_DampsCollisionStorm(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "takeover-test",
		Clock:      clk,
	}, connectivity.SessionExclusive, nil, rec)

	// Simulate the connection having just come up.
	sess.mu.Lock()
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()

	require.Equal(t, time.Duration(0), sess.takeoverPenalty(), "no penalty before any takeover")

	wantPenalties := []time.Duration{
		0,               // 1st takeover: legitimate failover, standby must not be slowed
		1 * time.Second, // 2nd
		2 * time.Second, // 3rd
		4 * time.Second, // 4th
	}
	for i, want := range wantPenalties {
		// Each takeover happens shortly after the previous reconnect —
		// well inside the stability window — so the streak accumulates.
		clk.Advance(2 * time.Second)
		sess.handleServerDisconnect(disconnectSessionTakenOver)
		require.Equal(t, want, sess.takeoverPenalty(), "penalty after takeover #%d", i+1)
	}

	// The penalty is capped at 64s even for an absurd streak.
	for i := 0; i < 20; i++ {
		clk.Advance(time.Second)
		sess.handleServerDisconnect(disconnectSessionTakenOver)
	}
	require.Equal(t, 64*time.Second, sess.takeoverPenalty(), "penalty must cap at 64s")

	entries := rec.FindEntries(MetricMQTTSessionTakeover)
	require.Len(t, entries, len(wantPenalties)+20, "every takeover must be counted")
}

// TestNoteSessionTakeover_StableConnectionResetsStreak verifies a
// connection that stayed up for the stability window resets the streak:
// the next takeover is treated as a fresh (legitimate) failover with no
// penalty.
func TestNoteSessionTakeover_StableConnectionResetsStreak(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "takeover-reset",
		Clock:      clk,
	}, connectivity.SessionExclusive, nil)

	// Build up a streak (storm in progress).
	sess.mu.Lock()
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()
	for i := 0; i < 3; i++ {
		clk.Advance(time.Second)
		sess.handleServerDisconnect(disconnectSessionTakenOver)
	}
	require.Greater(t, sess.takeoverPenalty(), time.Duration(0), "storm must carry a penalty")

	// Reconnect and stay stable past the window.
	sess.mu.Lock()
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()
	clk.Advance(takeoverStabilityWindow + time.Second)

	sess.handleServerDisconnect(disconnectSessionTakenOver)
	require.Equal(t, time.Duration(0), sess.takeoverPenalty(),
		"a takeover after a stable connection is a fresh failover: no penalty")
}

// TestHandleServerDisconnect_NonTakeoverCodesDoNotCountTakeovers pins
// that only 0x8E feeds the takeover damping.
func TestHandleServerDisconnect_NonTakeoverCodesDoNotCountTakeovers(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "takeover-other-codes",
	}, connectivity.SessionPersistent, nil, rec)

	sess.handleServerDisconnect(0x89) // server busy
	sess.handleServerDisconnect(0x8B) // server shutting down
	sess.handleServerDisconnect(0x00) // normal

	require.Empty(t, rec.FindEntries(MetricMQTTSessionTakeover))
	require.Equal(t, time.Duration(0), sess.takeoverPenalty())
}
