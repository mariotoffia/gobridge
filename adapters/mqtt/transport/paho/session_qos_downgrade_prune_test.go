package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A downgrade record lives exactly as long as a plan wants its filter at the
// recorded requested QoS. It is dropped when Reconcile stashes a plan that does
// not — before any broker operation, so neither a reconcile that fails nor the
// empty-plan no-op can leave the gauge and health reporting a filter that is
// gone.

// twoFilterPlan wants sensors/x and sensors/y at QoS 1.
func twoFilterPlan() connectivity.SessionPlan {
	return connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{
		{Topic: "sensors/x", QoS: 1},
		{Topic: "sensors/y", QoS: 1},
	}}
}

// reconnectReset runs the connection-up edge of a reconnect, which resets the
// broker-observed and contract-active subscription state.
func reconnectReset(s *Session) {
	s.mu.Lock()
	generation := s.connectionGeneration
	s.mu.Unlock()
	s.handleConnectionUpGeneration(generation)
}

// recordFailedDowngrade reconciles sensors/x granted 0 alongside a refused
// sensors/y, so the reconcile fails and the applied plan stays empty while x
// holds a downgrade record.
func recordFailedDowngrade(t *testing.T, s *Session, fake *fakeReconcileConn) {
	t.Helper()
	fake.setTopicReasons(map[string]byte{"sensors/x": 0x00, "sensors/y": 0x87})
	require.ErrorIs(t, s.Reconcile(context.Background(), twoFilterPlan()), shared.ErrForbidden)
	_, recorded := downgradeState(s, "sensors/x")
	require.True(t, recorded)
}

// TestQoSDowngrade_EmptyPlanNoOp_DropsAcceptedRecord: the accepted filter came
// from a reconcile that failed, so the applied plan is still empty, and after
// a reconnect the empty plan takes the no-op path that never reaches the
// end-of-reconcile alignment.
func TestQoSDowngrade_EmptyPlanNoOp_DropsAcceptedRecord(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-noop-accepted", connectivity.SessionPersistent, 0x00, nil)
	recordFailedDowngrade(t, s, fake)
	confirmDowngrade(t, s, clk, fake, "sensors/x")
	requireGauge(t, rec, "downgrade-noop-accepted", 1)
	reconnectReset(s)

	require.NoError(t, s.Reconcile(ctx, connectivity.SessionPlan{}))
	requireGauge(t, rec, "downgrade-noop-accepted", 0)
	require.Empty(t, s.Health(ctx).BestEffortTopics)
}

// TestQoSDowngrade_EmptyPlanNoOp_SenderOnlySessionReachesFull: a record still
// confirming when the empty plan arrives must not keep a sender-only session
// below Full.
func TestQoSDowngrade_EmptyPlanNoOp_SenderOnlySessionReachesFull(t *testing.T) {
	ctx := context.Background()
	s, fake, _, _ := newDowngradeSession(t, "downgrade-noop-confirming", connectivity.SessionPersistent, 0x00, nil)
	recordFailedDowngrade(t, s, fake)
	reconnectReset(s)

	require.NoError(t, s.Reconcile(ctx, connectivity.SessionPlan{}))
	_, recorded := downgradeState(s, "sensors/x")
	require.False(t, recorded)
	require.Equal(t, ports.ServiceLevelFull, s.Health(ctx).ServiceLevel)
}

// TestQoSDowngrade_FailedReconcileOfNewPlan_DropsRemovedFilter: the reconcile
// unsubscribes the accepted filter and then fails on the plan's new one.
func TestQoSDowngrade_FailedReconcileOfNewPlan_DropsRemovedFilter(t *testing.T) {
	ctx := context.Background()
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-replaced", connectivity.SessionPersistent, 0x00, nil)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	fake.setTopicReasons(map[string]byte{"sensors/y": 0x87})
	require.ErrorIs(t, s.Reconcile(ctx, planAtQoS("sensors/y", 1)), shared.ErrForbidden)
	require.Equal(t, 1, fake.unsubscribeCallCount(), "sensors/x was unsubscribed")
	requireGauge(t, rec, "downgrade-replaced", 0)
	require.Empty(t, s.Health(ctx).BestEffortTopics)
}

// TestQoSDowngrade_ObservedLowerGrantWithoutRecord_IsResubscribed pins the
// invariant that an observed lower grant always has a record. The seeded state
// is what a dropped record leaves when its filter's UNSUBSCRIBE failed and a
// later plan wants the filter again: without a record nothing would ever
// re-evaluate the grant.
func TestQoSDowngrade_ObservedLowerGrantWithoutRecord_IsResubscribed(t *testing.T) {
	s, fake, _, _ := newDowngradeSession(t, "downgrade-self-heal", connectivity.SessionPersistent, 0x00, nil)
	s.mu.Lock()
	s.observedSubs["sensors/x"] = subscriptionGrant{Requested: 1, Granted: 0}
	s.mu.Unlock()

	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)))
	require.Equal(t, 1, fake.subscribeCallCount(), "a fresh SUBSCRIBE re-evaluates the grant")
	d, recorded := downgradeState(s, "sensors/x")
	require.True(t, recorded)
	require.Equal(t, 1, d.confirmations)
}

// TestQoSDowngrade_RouteLoweredToGrantedQoS_ClearsWithoutRecoveryLog covers the
// operator's fix: lowering the route's qos to the granted level. The record is
// dropped with the old plan, so nothing claims the broker "grants the requested
// QoS again" — it never did.
func TestQoSDowngrade_RouteLoweredToGrantedQoS_ClearsWithoutRecoveryLog(t *testing.T) {
	ctx := context.Background()
	logs := &recordingLogHandler{}
	s, fake, clk, rec := newDowngradeSession(t, "downgrade-lowered", connectivity.SessionPersistent, 0x00, logs)
	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 1)))
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	require.NoError(t, s.Reconcile(ctx, planAtQoS("sensors/x", 0)))
	qos, active := activeQoS(s, "sensors/x")
	require.True(t, active)
	require.Equal(t, byte(0), qos)
	requireGauge(t, rec, "downgrade-lowered", 0)
	require.Zero(t, logs.messageCountContaining(slog.LevelInfo, "requested subscription QoS again"))
}
