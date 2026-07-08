package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/ports"
)

// Finding C3-M (readiness): a deferred-connect standby's source session never
// connects until this instance wins the lease. The pre-fix readiness derivation
// required every session connected, so such a standby was pinned at LevelRunning
// and a level=connected readiness probe marked it permanently unready — ejecting
// the only failover target. The fix excludes a deferred-connect session that
// holds no lease from the connectivity aggregate so the standby reaches its
// LevelSubscribed cap.
func TestReadinessFromSnapshot_DeferredConnectStandbyNotPinnedAtRunning(t *testing.T) {
	dh := ports.DeepHealth{
		Running: true,
		Healthy: true,
		Sessions: []ports.SessionHealthDetail{
			{
				SessionID:         "sess-standby",
				Connected:         false, // deferred: not connected until lease won
				HasLease:          false,
				ConnectAfterLease: true,
			},
		},
	}

	level := ports.ReadinessLevelFromDeepHealth(dh)
	assert.Greater(t, int(level), int(ports.LevelRunning),
		"a deferred-connect standby must not be pinned at LevelRunning")
	assert.GreaterOrEqual(t, int(level), int(ports.LevelSubscribed),
		"a ready deferred-connect standby should reach at least LevelSubscribed")
}

// Guard: the deferred-connect exemption must NOT leak to ordinary sessions. A
// normal disconnected session (or a deferred one that DOES hold the lease, i.e.
// the active instance) still gates readiness at LevelRunning.
func TestReadinessFromSnapshot_NonDeferredDisconnectedStillGated(t *testing.T) {
	// Ordinary disconnected session.
	assert.Equal(t, ports.LevelRunning, ports.ReadinessLevelFromDeepHealth(ports.DeepHealth{
		Running:  true,
		Healthy:  true,
		Sessions: []ports.SessionHealthDetail{{SessionID: "s", Connected: false}},
	}))

	// Deferred-connect session that WON the lease (active) must be connected; a
	// disconnected active session is a genuine fault, so it stays gated.
	assert.Equal(t, ports.LevelRunning, ports.ReadinessLevelFromDeepHealth(ports.DeepHealth{
		Running: true,
		Healthy: true,
		Sessions: []ports.SessionHealthDetail{
			{SessionID: "s", Connected: false, HasLease: true, ConnectAfterLease: true},
		},
	}))
}
