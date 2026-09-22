package paho

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// The probe re-sends SUBSCRIBE for a downgraded filter: every
// qosDowngradeConfirmInterval while the grant is being confirmed, then every
// qos_recheck_interval once it is accepted as best effort.

// TestQoSDowngrade_ConfirmationSubscribesUseRetainHandlingOne proves a probe
// never triggers a retained replay: the subscription already exists, so even an
// ephemeral session (whose reconcile uses RetainHandling 0) re-subscribes with 1.
func TestQoSDowngrade_ConfirmationSubscribesUseRetainHandlingOne(t *testing.T) {
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-retain", connectivity.SessionEphemeral, 0x00, nil)
	s.opts.NoLocal = true
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	calls := fake.subscribeSpecs()
	require.Len(t, calls, qosDowngradeConfirmations)
	first := subscribeSpec{Topic: "sensors/x", QoS: 1, NoLocal: true, RetainHandling: 0}
	require.Equal(t, []subscribeSpec{first}, calls[0], "reconcile of an ephemeral session")
	probe := first
	probe.RetainHandling = 1
	for i, call := range calls[1:] {
		require.Equal(t, []subscribeSpec{probe}, call, "confirmation SUBSCRIBE %d", i+2)
	}
}

// TestQoSDowngrade_RecheckGrantingRequestedQoS_RestoresSubscription proves a
// lifted broker cap is noticed by the re-check and clears the best effort.
func TestQoSDowngrade_RecheckGrantingRequestedQoS_RestoresSubscription(t *testing.T) {
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-recovered", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(ctx, withReceiver(s, planAtQoS("sensors/x", 1))))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	fake.setReasons([]byte{0x01})
	advanceAndAwait(t, s, clk, DefaultQoSRecheckInterval, "requested QoS granted again", func() bool {
		qos, active := activeQoS(s, "sensors/x")
		_, recorded := downgradeState(s, "sensors/x")
		return active && qos == 1 && !recorded
	})
	require.Equal(t, qosDowngradeConfirmations+1, fake.subscribeCallCount())
	requireGauge(t, rec, "downgrade-recovered", 0)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelInfo, "requested subscription QoS again"))
	h := s.Health(ctx)
	require.Empty(t, h.BestEffortTopics)
	require.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
}

// TestQoSDowngrade_RecheckStillLower_ChangesNothing proves a re-check that gets
// the same lower grant is silent and keeps its cadence.
func TestQoSDowngrade_RecheckStillLower_ChangesNothing(t *testing.T) {
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-still", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	for subscribes := qosDowngradeConfirmations + 1; subscribes <= qosDowngradeConfirmations+2; subscribes++ {
		advanceAndAwait(t, s, clk, DefaultQoSRecheckInterval, "re-check rescheduled", func() bool {
			d, ok := downgradeState(s, "sensors/x")
			return ok && fake.subscribeCallCount() == subscribes &&
				d.due.Equal(clk.Now().Add(DefaultQoSRecheckInterval))
		})
		d, _ := downgradeState(s, "sensors/x")
		require.True(t, d.accepted())
		require.Len(t, rec.FindEntries(MetricMQTTQoSDowngradedActive), 1, "the gauge moves only on a change")
		require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"))
		require.Len(t, rec.FindEntries(MetricMQTTQoSDowngraded), 1)
	}
}

// TestQoSDowngrade_RecheckIntervalZero_SendsNoRecheck proves
// qos_recheck_interval 0 turns the re-check off once the grant is accepted.
func TestQoSDowngrade_RecheckIntervalZero_SendsNoRecheck(t *testing.T) {
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-no-recheck", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planWithRecheck("sensors/x", 1, 0)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	d, _ := downgradeState(s, "sensors/x")
	require.True(t, d.due.IsZero(), "nothing is scheduled")
	awaitNoTimer(t, clk)
	for range 24 {
		clk.Advance(time.Hour)
	}
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount())
}

// TestQoSDowngrade_ChangedRecheckInterval_AppliesWithoutResubscribe proves a
// reloaded qos_recheck_interval reaches an accepted downgrade although reconcile
// does not re-subscribe the unchanged filter.
func TestQoSDowngrade_ChangedRecheckInterval_AppliesWithoutResubscribe(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-reload", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	require.NoError(t, s.Reconcile(ctx, planWithRecheck("sensors/x", 1, 0)))
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount(), "no SUBSCRIBE from the reload")
	d, _ := downgradeState(s, "sensors/x")
	require.True(t, d.due.IsZero())
	awaitNoTimer(t, clk)
	clk.Advance(2 * time.Hour)
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount())

	require.NoError(t, s.Reconcile(ctx, planWithRecheck("sensors/x", 1, 2*time.Hour)))
	advanceAndAwait(t, s, clk, 2*time.Hour, "re-check at the new interval", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && fake.subscribeCallCount() == qosDowngradeConfirmations+1 &&
			d.due.Equal(clk.Now().Add(2*time.Hour))
	})
}

// TestQoSDowngrade_ProbeRefused_KeepsGrantAndRetries proves a probe that gets no
// grant keeps the recorded one and asks again, instead of dropping the filter.
func TestQoSDowngrade_ProbeRefused_KeepsGrantAndRetries(t *testing.T) {
	logs := &recordingLogHandler{}
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-probe-refused", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))

	fake.setReasons([]byte{0x87})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "refused probe rescheduled", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && fake.subscribeCallCount() == 2 &&
			d.due.Equal(clk.Now().Add(qosDowngradeConfirmInterval))
	})
	d, _ := downgradeState(s, "sensors/x")
	require.Equal(t, 1, d.confirmations, "a refusal is no confirmation")
	require.Equal(t, byte(0), d.granted)
	require.Equal(t, 1, logs.warnCountContaining("re-check SUBSCRIBE got no grant"))

	fake.setReasons([]byte{0x00})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "confirmation after the refusal", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.confirmations == 2
	})
}
