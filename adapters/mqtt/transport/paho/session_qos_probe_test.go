package paho

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
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
		// Health is never called here, so every sample is an on-change write.
		require.Len(t, rec.FindEntries(MetricMQTTQoSDowngradedActive), 1, "an unchanged count writes no sample")
		requireGauge(t, rec, "downgrade-still", 1)
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

// TestQoSDowngrade_ProbeRefusedRepeatedly_WarnsOncePerStreak proves a broker
// that keeps refusing the probe SUBSCRIBE is reported once per run of refusals,
// not on every retry.
func TestQoSDowngrade_ProbeRefusedRepeatedly_WarnsOncePerStreak(t *testing.T) {
	const warning = "re-check SUBSCRIBE got no grant"
	logs := &recordingLogHandler{}
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-refused-streak", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))

	fake.setReasons([]byte{0x87})
	refusedProbeRound(t, s, clk, fake, 2, qosDowngradeConfirmInterval, qosDowngradeConfirmInterval)
	refusedProbeRound(t, s, clk, fake, 3, qosDowngradeConfirmInterval, 2*qosDowngradeConfirmInterval)
	require.Equal(t, 1, logs.warnCountContaining(warning), "one Warn for the whole streak")

	fake.setReasons([]byte{0x00})
	advanceAndAwait(t, s, clk, 2*qosDowngradeConfirmInterval, "a grant ends the streak", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.confirmations == 2
	})
	fake.setReasons([]byte{0x87})
	refusedProbeRound(t, s, clk, fake, 5, qosDowngradeConfirmInterval, qosDowngradeConfirmInterval)
	require.Equal(t, 2, logs.warnCountContaining(warning), "a new streak warns again")
}

// noGrantWarning is the stable text of the probe's no-verdict Warn.
const noGrantWarning = "re-check SUBSCRIBE got no grant"

// noGrantWarnings returns the attributes of every no-verdict Warn, in order.
func noGrantWarnings(logs *recordingLogHandler) []map[string]any {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	var out []map[string]any
	for _, r := range logs.records {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, noGrantWarning) {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		out = append(out, attrs)
	}
	return out
}

// requireNoGrantWarning asserts one no-verdict Warn names topic and carries a
// cause classified as want.
func requireNoGrantWarning(t *testing.T, attrs map[string]any, clientID, topic string, want error) {
	t.Helper()
	require.Equal(t, clientID, attrs["client_id"])
	require.Equal(t, topic, attrs["topic"])
	cause, ok := attrs["error"].(error)
	require.True(t, ok, "the Warn carries its cause as an error, got %T", attrs["error"])
	require.ErrorIs(t, cause, want)
}

// TestQoSDowngrade_ProbeWithoutVerdict_NamesEachFilterWhoseStreakStarts proves
// every filter whose run of no-verdict probes starts in a round is named in its
// own Warn, with its own cause. Warning only for the round's first refused
// filter would misattribute a later filter's streak — and never name that
// filter at all, since its next rounds are not a streak's first.
func TestQoSDowngrade_ProbeWithoutVerdict_NamesEachFilterWhoseStreakStarts(t *testing.T) {
	const clientID = "downgrade-refused-per-filter"
	logs := &recordingLogHandler{}
	s, fake, clk, _ := newDowngradeSession(t, clientID, connectivity.SessionPersistent, 0x00, logs)
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{
		{Topic: "sensors/a", QoS: 1},
		{Topic: "sensors/b", QoS: 1},
	}}
	fake.setTopicReasons(map[string]byte{"sensors/a": 0x00, "sensors/b": 0x00})
	require.NoError(t, s.Reconcile(context.Background(), plan))

	fake.setTopicReasons(map[string]byte{"sensors/a": 0x87, "sensors/b": 0x00})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "only sensors/a refused", func() bool {
		a, okA := downgradeState(s, "sensors/a")
		b, okB := downgradeState(s, "sensors/b")
		return okA && okB && a.noVerdictRounds == 1 && b.confirmations == 2
	})
	warns := noGrantWarnings(logs)
	require.Len(t, warns, 1)
	requireNoGrantWarning(t, warns[0], clientID, "sensors/a", shared.ErrForbidden)

	fake.setTopicReasons(map[string]byte{"sensors/a": 0x87, "sensors/b": 0x97})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "both refused", func() bool {
		a, okA := downgradeState(s, "sensors/a")
		b, okB := downgradeState(s, "sensors/b")
		return okA && okB && a.noVerdictRounds == 2 && b.noVerdictRounds == 1
	})
	warns = noGrantWarnings(logs)
	require.Len(t, warns, 2, "sensors/a's streak continues silently; sensors/b's starts")
	requireNoGrantWarning(t, warns[1], clientID, "sensors/b", shared.ErrThrottled)
}

// TestNoGrantCause_ClassifiesEachWayAProbeGetsNoGrant pins the cause a
// no-verdict Warn names for each shape of broker answer.
func TestNoGrantCause_ClassifiesEachWayAProbeGetsNoGrant(t *testing.T) {
	require.ErrorIs(t, noGrantCause(1, []byte{0x00, 0x87}, nil), shared.ErrForbidden, "a refusal reason code")
	require.ErrorIs(t, noGrantCause(1, []byte{0x00}, nil), shared.ErrProtocolError, "a short SUBACK")
	require.ErrorIs(t, noGrantCause(0, nil, context.DeadlineExceeded), shared.ErrTimeout, "no SUBACK at all")
}

// refusedProbeRound advances the clock by advance so the due probe runs and
// gets no grant, then waits until `subscribes` SUBSCRIBEs have been sent in
// total and the next probe is due `next` from now.
func refusedProbeRound(
	t *testing.T, s *Session, clk *clocktest.Fake, fake *fakeReconcileConn,
	subscribes int, advance, next time.Duration,
) {
	t.Helper()
	advanceAndAwait(t, s, clk, advance, fmt.Sprintf("refused probe %d rescheduled %s ahead", subscribes, next), func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && fake.subscribeCallCount() == subscribes && d.due.Equal(clk.Now().Add(next))
	})
}

// TestQoSDowngrade_ProbeWithoutVerdict_BacksOffWhileConfirming proves a broker
// that keeps refusing the confirmation SUBSCRIBE is asked less and less often:
// each consecutive round without a grant doubles the wait, up to
// MinQoSRecheckInterval, and a grant resets it.
func TestQoSDowngrade_ProbeWithoutVerdict_BacksOffWhileConfirming(t *testing.T) {
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-backoff", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))

	fake.setReasons([]byte{0x87})
	advance := qosDowngradeConfirmInterval
	delays := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for round, next := range delays {
		refusedProbeRound(t, s, clk, fake, round+2, advance, next)
		advance = next
	}

	fake.setReasons([]byte{0x00})
	advanceAndAwait(t, s, clk, advance, "a grant resets the backoff", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.confirmations == 2 && d.due.Equal(clk.Now().Add(qosDowngradeConfirmInterval))
	})
	fake.setReasons([]byte{0x87})
	refusedProbeRound(t, s, clk, fake, len(delays)+3, qosDowngradeConfirmInterval, qosDowngradeConfirmInterval)
}

// TestQoSDowngrade_AcceptedProbeWithoutVerdict_KeepsRecheckInterval proves the
// backoff is for confirmation only: a re-check of an accepted downgrade that
// gets no grant retries at qos_recheck_interval, never sooner.
func TestQoSDowngrade_AcceptedProbeWithoutVerdict_KeepsRecheckInterval(t *testing.T) {
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-accepted-refused", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	fake.setReasons([]byte{0x87})
	for round := 1; round <= 2; round++ {
		refusedProbeRound(t, s, clk, fake, qosDowngradeConfirmations+round,
			DefaultQoSRecheckInterval, DefaultQoSRecheckInterval)
	}
	d, _ := downgradeState(s, "sensors/x")
	require.True(t, d.accepted(), "a re-check without a verdict keeps the accepted grant")
}
