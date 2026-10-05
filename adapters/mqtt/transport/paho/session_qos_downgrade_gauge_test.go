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

// TestQoSDowngrade_StandingDowngrade_RewritesGaugeEveryInterval proves a
// standing downgrade keeps producing samples with no Health caller. The
// CloudWatch exporter publishes each gauge call as one datapoint, and nothing
// guarantees a periodic Health caller; without the re-write an alarm on the
// gauge could see one sample and fall back to OK while the downgrade still
// stands.
func TestQoSDowngrade_StandingDowngrade_RewritesGaugeEveryInterval(t *testing.T) {
	const clientID = "downgrade-gauge-rewrite"
	s, fake, clk, rec := newDowngradeSession(t, clientID, connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")
	requireGauge(t, rec, clientID, 1)

	for range 3 {
		samples := len(rec.FindEntries(MetricMQTTQoSDowngradedActive))
		clk.Advance(qosDowngradeGaugeInterval)
		wait.Until(t, 5*time.Second, "the standing downgrade is re-written", func() bool {
			return len(rec.FindEntries(MetricMQTTQoSDowngradedActive)) > samples
		})
		requireGauge(t, rec, clientID, 1)
	}
}

// TestQoSDowngrade_GaugeRewrite_StopsWhenNoDowngradeStands proves the re-write
// runs only while a downgrade stands: once the count falls to zero the last
// sample is 0 and no later tick writes another. A re-write that outlived the
// downgrade would hold the alarm raised after the cause was fixed.
func TestQoSDowngrade_GaugeRewrite_StopsWhenNoDowngradeStands(t *testing.T) {
	for _, c := range []struct {
		name  string
		clear func(t *testing.T, s *Session)
	}{
		{"subscription removed from plan", func(t *testing.T, s *Session) {
			require.NoError(t, s.Reconcile(context.Background(), connectivity.SessionPlan{}))
		}},
		{"session closed", func(t *testing.T, s *Session) {
			require.NoError(t, s.Close(context.Background()))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			const clientID = "downgrade-gauge-rewrite-stop"
			s, fake, clk, rec := newDowngradeSession(t, clientID, connectivity.SessionPersistent, 0x00, nil)
			idle := clk.TickerCount()
			require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
			confirmDowngrade(t, s, clk, fake, "sensors/x")
			require.Equal(t, idle+1, clk.TickerCount(), "a standing downgrade arms one re-write")

			c.clear(t, s)
			requireGauge(t, rec, clientID, 0)
			wait.Until(t, 5*time.Second, "the re-write stopped", func() bool { return clk.TickerCount() == idle })
			samples := len(rec.FindEntries(MetricMQTTQoSDowngradedActive))
			clk.Advance(3 * qosDowngradeGaugeInterval)
			require.Len(t, rec.FindEntries(MetricMQTTQoSDowngradedActive), samples,
				"no sample once no downgrade stands")
			requireGauge(t, rec, clientID, 0)
		})
	}
}
