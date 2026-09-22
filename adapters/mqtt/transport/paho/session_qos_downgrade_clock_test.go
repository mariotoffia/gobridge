package paho

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// TestQoSDowngrade_ZeroTimeClock_AcceptsAfterThreeConfirmations proves
// acceptance counts fresh SUBACKs, not the clock. The clock.Clock contract lets
// Now return the zero time (clocktest.NewAt(time.Time{})). While the recorded
// acceptance time doubled as the acceptance flag, a grant confirmed at that
// instant stayed confirming forever: never active, never gauged, and an Error
// on every later SUBACK.
//
// The fresh SUBACKs come from reconnect reconciles, which re-subscribe every
// filter without the clock moving, so all three arrive at the zero instant.
func TestQoSDowngrade_ZeroTimeClock_AcceptsAfterThreeConfirmations(t *testing.T) {
	const clientID = "downgrade-zero-clock"
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, _, rec := newDowngradeSessionOnClock(t, clocktest.NewAt(time.Time{}), clientID,
		connectivity.SessionPersistent, 0x00, logs)
	plan := planAtQoS("sensors/x", 1)

	require.NoError(t, s.Reconcile(ctx, plan))
	for range qosDowngradeConfirmations - 1 {
		reconnectReset(s)
		require.NoError(t, s.Reconcile(ctx, plan))
	}
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount(), "each reconcile got a fresh SUBACK")

	qos, active := activeQoS(s, "sensors/x")
	require.True(t, active, "an accepted downgrade is contract-active")
	require.Equal(t, byte(0), qos, "at the granted QoS")
	requireGauge(t, rec, clientID, 1)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"))

	reconnectReset(s)
	require.NoError(t, s.Reconcile(ctx, plan))
	require.Equal(t, qosDowngradeConfirmations+1, fake.subscribeCallCount())
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"),
		"a later SUBACK with the same grant logs no new Error")
	requireGauge(t, rec, clientID, 1)
}
