package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A broker may grant a subscription a lower QoS than requested. The session
// confirms the grant with fresh SUBSCRIBEs qosDowngradeConfirmInterval apart;
// once qosDowngradeConfirmations fresh SUBACKs agree, the subscription is kept
// active at the granted QoS as best effort and re-checked every
// qos_recheck_interval. A lower grant never fails the reconcile and never stops
// the session — the old behaviour escalated three identical (and stale)
// reconciles to a permanent closure that restarted the whole process.

// planAtQoS is the one-subscription plan these tests reconcile. It carries no
// Paho config, so the subscription re-checks at DefaultQoSRecheckInterval.
func planAtQoS(topic string, qos int) connectivity.SessionPlan {
	return connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: qos}},
	}
}

// planWithRecheck is planAtQoS with an explicit qos_recheck_interval.
func planWithRecheck(topic string, qos int, recheck time.Duration) connectivity.SessionPlan {
	plan := planAtQoS(topic, qos)
	plan.Subscriptions[0].Config = &Config{Subscription: SubscriptionOptions{QoSRecheckInterval: recheck}}
	return plan
}

// newDowngradeSession returns a connected session on a fake clock whose every
// SUBSCRIBE is answered with the single reason code granted. The session is
// closed when the test ends, which also stops any scheduled probe.
func newDowngradeSession(
	tb testing.TB,
	clientID string,
	mode connectivity.SessionMode,
	granted byte,
	logs slog.Handler,
) (*Session, *fakeReconcileConn, *clocktest.Fake, *ports.RecordingExporter) {
	tb.Helper()
	clk := testClock()
	fake := &fakeReconcileConn{reasons: []byte{granted}}
	rec := &ports.RecordingExporter{}
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(logs)
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   clientID,
		Clock:      clk,
	}, mode, logger, rec)
	tb.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = fake
	s.connected = true
	empty := connectivity.SessionPlan{}
	s.appliedPlan = &empty
	s.mu.Unlock()
	return s, fake, clk, rec
}

// withReceiver registers a receiver handler and names it in the plan, so the
// session can reach ServiceLevelFull.
func withReceiver(s *Session, plan connectivity.SessionPlan) connectivity.SessionPlan {
	s.router.Register("rx-sensors", func(*pahov5.Publish) {})
	plan.ExpectedReceiverIDs = []string{"rx-sensors"}
	return plan
}

// downgradeState returns a copy of the downgrade record for topic.
func downgradeState(s *Session, topic string) (qosDowngrade, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.qosDowngrades[topic]
	if !ok {
		return qosDowngrade{}, false
	}
	return *d, true
}

// activeQoS returns the contract-active QoS of topic.
func activeQoS(s *Session, topic string) (byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	qos, ok := s.activeSubs[topic]
	return qos, ok
}

// advanceAndAwait advances the fake clock and waits until cond holds, then
// until the probe that fired has released the session's serialization gate.
// The probe records a grant and schedules the next one in one locked section,
// so once cond sees the new state the next timer is registered; and it holds
// the gate until its logs and counters are out, so they are asserted
// deterministically.
func advanceAndAwait(tb testing.TB, s *Session, clk *clocktest.Fake, d time.Duration, desc string, cond func() bool) {
	tb.Helper()
	clk.Advance(d)
	if !wait.Poll(5*time.Second, cond) {
		tb.Fatalf("after advancing %s: %s never held", d, desc)
	}
	awaitReloadGate(tb, s, 5*time.Second, "await probe completion")
}

// awaitReloadGate takes and releases the session's serialization gate, so the
// reconcile or probe that held it has finished, its logs and counters
// included. It fails the test when the gate is still held after bound.
func awaitReloadGate(tb testing.TB, s *Session, bound time.Duration, what string) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), bound)
	defer cancel()
	if err := s.acquireReload(ctx); err != nil {
		tb.Fatalf("%s: the session's serialization gate was still held after %s: %v", what, bound, err)
	}
	s.releaseReload()
}

// confirmDowngrade drives the confirmation SUBSCRIBEs after the reconcile that
// first reported a lower grant for topic, until the grant is accepted.
func confirmDowngrade(tb testing.TB, s *Session, clk *clocktest.Fake, fake *fakeReconcileConn, topic string) {
	tb.Helper()
	for n := 2; n <= qosDowngradeConfirmations; n++ {
		subscribes := fake.subscribeCallCount() + 1
		advanceAndAwait(tb, s, clk, qosDowngradeConfirmInterval, fmt.Sprintf("confirmation %d of %q", n, topic), func() bool {
			d, ok := downgradeState(s, topic)
			return ok && d.confirmations == n && fake.subscribeCallCount() == subscribes
		})
	}
	if d, ok := downgradeState(s, topic); !ok || !d.accepted() {
		tb.Fatalf("%q not accepted after %d confirmations: %+v", topic, qosDowngradeConfirmations, d)
	}
}

// awaitNoTimer waits until no fake timer is pending, so an Advance afterwards
// provably cannot start a probe. A replaced schedule stops its timer from its
// own goroutine, which is why this waits instead of asserting.
func awaitNoTimer(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	wait.Until(t, 5*time.Second, "no pending probe timer", func() bool { return clk.TimerCount() == 0 })
}

// requireGauge asserts the last MQTTQoSDowngradedActive value and its tag.
func requireGauge(t *testing.T, rec *ports.RecordingExporter, clientID string, want float64) {
	t.Helper()
	entries := rec.FindEntries(MetricMQTTQoSDowngradedActive)
	require.NotEmpty(t, entries, "MQTTQoSDowngradedActive was never emitted")
	last := entries[len(entries)-1]
	require.Equal(t, "gauge", last.Kind)
	require.Equal(t, want, last.FValue)
	require.Contains(t, last.Tags, shared.Tag{Key: shared.TagKeySessionID, Value: clientID})
}

// TestQoSDowngrade_ThreeFreshIdenticalGrants_AcceptedAsBestEffort pins the
// lifecycle: a lower grant is confirmed by fresh SUBSCRIBEs, then kept active at
// the granted QoS. The old code returned ErrQoSNotSupported from every reconcile
// and escalated the third to ErrTransportClosedPermanently.
func TestQoSDowngrade_ThreeFreshIdenticalGrants_AcceptedAsBestEffort(t *testing.T) {
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-accept", connectivity.SessionPersistent, 0x00, logs)
	plan := withReceiver(s, planAtQoS("sensors/x", 1))

	require.NoError(t, s.Reconcile(ctx, plan), "a lower grant must not fail the reconcile")
	require.Equal(t, 1, fake.subscribeCallCount())

	confirmDowngrade(t, s, clk, fake, "sensors/x")
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount(),
		"each confirmation is a fresh SUBSCRIBE")

	qos, active := activeQoS(s, "sensors/x")
	require.True(t, active, "an accepted downgrade is contract-active")
	require.Equal(t, byte(0), qos, "at the granted QoS")
	requireGauge(t, rec, "downgrade-accept", 1)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"),
		"acceptance is announced once, at Error")
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "no acknowledgement or redelivery"),
		"a QoS 0 grant names what QoS 0 gives up")
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "retry_unsupported"),
		"a QoS 0 grant says a failed delivery goes to the DLQ or is dropped as retry_unsupported")
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "Configure a DLQ store"),
		"a QoS 0 grant recommends the DLQ its route was never required to have")
	require.Len(t, rec.FindEntries(MetricMQTTQoSDowngraded), 1,
		"the counter counts the first report, not the confirmations")

	h := s.Health(ctx)
	require.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
	require.Equal(t, []string{"sensors/x"}, h.BestEffortTopics)
	require.NotNil(t, h.SubscriptionsSatisfied)
	require.True(t, *h.SubscriptionsSatisfied)

	require.NoError(t, s.Reconcile(ctx, plan), "the accepted state is a converged state")
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount(),
		"an unchanged accepted filter is not re-subscribed by reconcile")
}

// TestQoSDowngrade_AcceptedAboveQoS0_LogsGrantedQoSDelivery proves the
// acceptance log describes the grant it accepts: the QoS 0 consequences (no
// acknowledgement, no redelivery, a failed delivery dropped as
// retry_unsupported without a DLQ) appear only when the broker granted QoS 0.
func TestQoSDowngrade_AcceptedAboveQoS0_LogsGrantedQoSDelivery(t *testing.T) {
	logs := &recordingLogHandler{}
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-qos1", connectivity.SessionPersistent, 0x01, logs)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 2)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"))
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError,
		"delivery runs at the granted QoS instead of the requested one"))
	require.Zero(t, logs.messageCountContaining(slog.LevelError, "no acknowledgement"),
		"QoS 0 consequences do not apply to a QoS 1 grant")
	require.Zero(t, logs.messageCountContaining(slog.LevelError, "retry_unsupported"),
		"a QoS 1 grant still redelivers, so a failed delivery is not dropped as retry_unsupported")
}

// TestQoSDowngrade_BestEffortTopics_AreSorted pins the documented order of
// SessionHealth.BestEffortTopics. The records live in a map, so Health is read
// repeatedly: an unsorted producer would return the map order at least once.
func TestQoSDowngrade_BestEffortTopics_AreSorted(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-sorted", connectivity.SessionPersistent, 0x00, nil)
	fake.setReasons([]byte{0x00, 0x00})
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{
		{Topic: "sensors/b", QoS: 1},
		{Topic: "sensors/a", QoS: 1},
	}}
	require.NoError(t, s.Reconcile(ctx, plan))

	for n := 2; n <= qosDowngradeConfirmations; n++ {
		advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, fmt.Sprintf("confirmation %d of both", n), func() bool {
			a, okA := downgradeState(s, "sensors/a")
			b, okB := downgradeState(s, "sensors/b")
			return okA && okB && a.confirmations == n && b.confirmations == n
		})
	}

	for range 32 {
		require.Equal(t, []string{"sensors/a", "sensors/b"}, s.Health(ctx).BestEffortTopics)
	}
}

// TestQoSDowngrade_WhileConfirming_HealthIsDegraded proves an unconfirmed lower
// grant is neither active nor reported as best effort.
func TestQoSDowngrade_WhileConfirming_HealthIsDegraded(t *testing.T) {
	ctx := context.Background()
	s, _, _, rec := newDowngradeSession(t, "downgrade-confirming", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, withReceiver(s, planAtQoS("sensors/x", 1))))

	h := s.Health(ctx)
	require.Equal(t, ports.ServiceLevelDegraded, h.ServiceLevel)
	require.NotNil(t, h.SubscriptionsSatisfied)
	require.False(t, *h.SubscriptionsSatisfied, "a downgrade still confirming is unsatisfied")
	require.Empty(t, h.BestEffortTopics)
	requireGauge(t, rec, "downgrade-confirming", 0)
}

// TestQoSDowngrade_Health_EmitsAcceptedCountEverySweep proves a standing
// downgrade keeps producing gauge samples. An exporter that publishes each gauge
// call as one datapoint (CloudWatch) would otherwise see a single sample, and an
// alarm on the gauge would fall to INSUFFICIENT_DATA while the downgrade stands.
func TestQoSDowngrade_Health_EmitsAcceptedCountEverySweep(t *testing.T) {
	ctx := context.Background()
	idle, _, _, idleRec := newDowngradeSession(t, "downgrade-health-none", connectivity.SessionPersistent, 0x00, nil)
	idle.Health(ctx)
	requireGauge(t, idleRec, "downgrade-health-none", 0)

	s, fake, clk, rec := newDowngradeSession(t, "downgrade-health-gauge", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")
	onChange := len(rec.FindEntries(MetricMQTTQoSDowngradedActive))
	for sweep := 1; sweep <= 3; sweep++ {
		s.Health(ctx)
		require.Len(t, rec.FindEntries(MetricMQTTQoSDowngradedActive), onChange+sweep, "one sample per Health sweep")
		requireGauge(t, rec, "downgrade-health-gauge", 1)
	}
}

// TestQoSDowngrade_RecoveryDuringConfirmation_NeverAccepted proves a transient
// cap that lifts before confirmation completes leaves no trace but the counter.
func TestQoSDowngrade_RecoveryDuringConfirmation_NeverAccepted(t *testing.T) {
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-transient", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(ctx, withReceiver(s, planAtQoS("sensors/x", 1))))

	fake.setReasons([]byte{0x01})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "requested QoS granted", func() bool {
		qos, active := activeQoS(s, "sensors/x")
		_, recorded := downgradeState(s, "sensors/x")
		return active && qos == 1 && !recorded
	})
	require.Equal(t, 2, fake.subscribeCallCount())
	require.Empty(t, rec.FindEntries(MetricMQTTQoSDowngradedActive), "never accepted, never gauged")
	require.Zero(t, logs.messageCountContaining(slog.LevelError, ""))
	require.Zero(t, logs.messageCountContaining(slog.LevelInfo, "requested subscription QoS again"),
		"a downgrade that was never accepted has nothing to clear")
	require.Equal(t, ports.ServiceLevelFull, s.Health(ctx).ServiceLevel)

	awaitNoTimer(t, clk)
	clk.Advance(DefaultQoSRecheckInterval)
	require.Equal(t, 2, fake.subscribeCallCount(), "a recovered subscription is not probed")
}

// TestQoSDowngrade_ChangedGrantRestartsConfirmation proves confirmation needs
// the SAME grant: a different lower grant is a new broker answer.
func TestQoSDowngrade_ChangedGrantRestartsConfirmation(t *testing.T) {
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-changed", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 2)))

	fake.setReasons([]byte{0x01})
	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "grant 1 recorded", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.granted == 1 && d.confirmations == 1
	})
	require.Len(t, rec.FindEntries(MetricMQTTQoSDowngraded), 2, "a changed grant is reported again")

	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "second confirmation", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.confirmations == 2
	})
	d, _ := downgradeState(s, "sensors/x")
	require.False(t, d.accepted(), "the earlier grant's confirmation does not carry over")

	advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, "third confirmation", func() bool {
		d, ok := downgradeState(s, "sensors/x")
		return ok && d.accepted()
	})
	require.Equal(t, 4, fake.subscribeCallCount())
	require.Len(t, rec.FindEntries(MetricMQTTQoSDowngraded), 2, "confirmations are not counted")
}

// TestQoSDowngrade_SubscriptionRemovedFromPlan_LowersGauge proves removing the
// route clears its best-effort state and stops its re-check.
func TestQoSDowngrade_SubscriptionRemovedFromPlan_LowersGauge(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-removed", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	require.NoError(t, s.Reconcile(ctx, connectivity.SessionPlan{}))
	require.Equal(t, 1, fake.unsubscribeCallCount())
	requireGauge(t, rec, "downgrade-removed", 0)
	require.Empty(t, s.Health(ctx).BestEffortTopics)
	_, recorded := downgradeState(s, "sensors/x")
	require.False(t, recorded)

	awaitNoTimer(t, clk)
	clk.Advance(DefaultQoSRecheckInterval)
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount(), "a removed filter is not re-checked")
}

// TestQoSDowngrade_Close_LowersGaugeAndStopsProbe proves a closed session does
// not keep reporting, or probing, a subscription that is gone.
func TestQoSDowngrade_Close_LowersGaugeAndStopsProbe(t *testing.T) {
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-close", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	require.NoError(t, s.Close(context.Background()))
	requireGauge(t, rec, "downgrade-close", 0)

	awaitNoTimer(t, clk)
	clk.Advance(2 * DefaultQoSRecheckInterval)
	require.Equal(t, qosDowngradeConfirmations, fake.subscribeCallCount())
}

// TestQoSDowngrade_Reconnect_ReevaluatesAcceptedGrant proves the reconnect's
// fresh SUBSCRIBE re-evaluates an accepted downgrade.
func TestQoSDowngrade_Reconnect_ReevaluatesAcceptedGrant(t *testing.T) {
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-reconnect", connectivity.SessionPersistent, 0x00, logs)
	plan := planAtQoS("sensors/x", 1)
	require.NoError(t, s.Reconcile(ctx, plan))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	s.mu.Lock()
	generation := s.connectionGeneration
	s.mu.Unlock()
	s.handleConnectionUpGeneration(generation)

	fake.setReasons([]byte{0x01})
	require.NoError(t, s.Reconcile(ctx, plan))
	require.Equal(t, qosDowngradeConfirmations+1, fake.subscribeCallCount(),
		"the reconnect reset the observed grant, so reconcile subscribes afresh")
	qos, active := activeQoS(s, "sensors/x")
	require.True(t, active)
	require.Equal(t, byte(1), qos)
	_, recorded := downgradeState(s, "sensors/x")
	require.False(t, recorded)
	requireGauge(t, rec, "downgrade-reconnect", 0)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelInfo, "requested subscription QoS again"))
}

// TestQoSDowngrade_ReconnectReset_HidesBestEffortUntilReactivated proves
// BestEffortTopics lists only contract-active filters: the reconnect reset
// deactivates every subscription until the reconnect's reconcile re-subscribes.
func TestQoSDowngrade_ReconnectReset_HidesBestEffortUntilReactivated(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, _ := newDowngradeSession(t, "downgrade-reconnect-hidden", connectivity.SessionPersistent, 0x00, nil)
	plan := planAtQoS("sensors/x", 1)
	require.NoError(t, s.Reconcile(ctx, plan))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	reconnectReset(s)
	require.Empty(t, s.Health(ctx).BestEffortTopics, "not active until re-subscribed")

	require.NoError(t, s.Reconcile(ctx, plan))
	require.Equal(t, []string{"sensors/x"}, s.Health(ctx).BestEffortTopics,
		"the same lower grant stays accepted")
}

// TestQoSDowngrade_RefusedSubscription_StillFailsReconcile is the control: a
// broker that REFUSES a subscription (SUBACK >= 0x80) fails that reconcile as
// before. Only a lower grant is accepted as best effort.
func TestQoSDowngrade_RefusedSubscription_StillFailsReconcile(t *testing.T) {
	s, _, _, rec := newDowngradeSession(t, "downgrade-refused", connectivity.SessionPersistent, 0x87, nil)

	err := s.Reconcile(context.Background(), planAtQoS("sensors/x", 1))
	require.ErrorIs(t, err, shared.ErrForbidden, "0x87 is not authorized")
	require.False(t, errors.Is(err, shared.ErrTransportClosedPermanently))
	_, recorded := downgradeState(s, "sensors/x")
	require.False(t, recorded, "a refusal is not a downgrade")
	require.Empty(t, rec.FindEntries(MetricMQTTQoSDowngradedActive))
	require.Empty(t, rec.FindEntries(MetricMQTTQoSDowngraded))
}

// TestReconcile_RejectsQoSRecheckIntervalBelowMinimum proves a plan handed
// straight to a session cannot schedule a re-check that loads the broker.
func TestReconcile_RejectsQoSRecheckIntervalBelowMinimum(t *testing.T) {
	for _, interval := range []time.Duration{30 * time.Second, -time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			s, fake, _, _ := newDowngradeSession(t, "downgrade-invalid", connectivity.SessionPersistent, 0x00, nil)
			err := s.Reconcile(context.Background(), planWithRecheck("sensors/x", 1, interval))
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
			require.Zero(t, fake.subscribeCallCount())
		})
	}
}
