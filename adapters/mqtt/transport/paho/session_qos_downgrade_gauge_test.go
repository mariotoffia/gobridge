package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Every MQTTQoSDowngradedActive sample is written under s.mu together with the
// count it reports, so a sample can never land after a later state change —
// a recovery, a plan removal or Close — has written its own. A Health sweep
// that counted before Close and wrote after it would leave a stale 1 standing,
// and after Close there may be no later sweep to correct it.

// TestQoSDowngrade_HealthRacingClose_LastGaugeSampleIsZero races Health sweeps
// against Close. Whatever the interleaving, a sweep either counts before Close
// (and Close's 0 follows) or after it (and counts 0), so the last sample is 0.
// The sweeps stop once Close starts, so no sweep that surely runs after Close
// hides a stale sample from one that overlapped it.
func TestQoSDowngrade_HealthRacingClose_LastGaugeSampleIsZero(t *testing.T) {
	const clientID = "downgrade-health-close-race"
	ctx := context.Background()
	s, fake, clk, rec := newDowngradeSession(t, clientID, connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")
	requireGauge(t, rec, clientID, 1)

	stop := make(chan struct{})
	swept := make(chan struct{})
	samples := len(rec.FindEntries(MetricMQTTQoSDowngradedActive))
	go func() {
		defer close(swept)
		for range 10_000 {
			select {
			case <-stop:
				return
			default:
			}
			s.Health(ctx)
		}
	}()
	wait.Until(t, 5*time.Second, "the sweeps are running", func() bool {
		return len(rec.FindEntries(MetricMQTTQoSDowngradedActive)) > samples
	})

	// The sweep in flight when stop closes overlaps Close; no later one starts.
	close(stop)
	require.NoError(t, s.Close(ctx))
	wait.RequireClosed(t, swept, 5*time.Second)
	requireGauge(t, rec, clientID, 0)
}

// TestQoSDowngrade_HealthAfterClose_EmitsZeroGauge proves a sweep of a closed
// session reports no accepted downgrade.
func TestQoSDowngrade_HealthAfterClose_EmitsZeroGauge(t *testing.T) {
	const clientID = "downgrade-health-after-close"
	ctx := context.Background()
	s, fake, clk, rec := newDowngradeSession(t, clientID, connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")
	require.NoError(t, s.Close(ctx))

	before := len(rec.FindEntries(MetricMQTTQoSDowngradedActive))
	s.Health(ctx)
	after := rec.FindEntries(MetricMQTTQoSDowngradedActive)
	require.Len(t, after, before+1, "one sample per sweep")
	require.Zero(t, after[before].FValue)
	requireGauge(t, rec, clientID, 0)
}
