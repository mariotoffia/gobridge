package ports_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// What "this member has converged" is allowed to mean.
//
// The readiness fold skips a session that waits for a lease it does not hold, so
// a warm standby can still report ready. Immediately after a config swap that
// description fits EVERY member of a lease-based cohort — the one about to take
// the lease has not re-acquired yet — so all of them fold to ready over an empty
// set. If that counted as convergence the confirm window would confirm a config
// none of them had tried, which is the one thing it exists to stop.

func satisfied(v bool) *bool { return &v }

func leaseSession(hasLease, connected, subscribed bool) ports.SessionHealthDetail {
	return ports.SessionHealthDetail{
		SessionID:              "mqtt",
		ConnectAfterLease:      true,
		HasLease:               hasLease,
		Connected:              connected,
		SubscriptionsSatisfied: satisfied(subscribed),
		ServiceLevel:           ports.ServiceLevelFull,
	}
}

func healthyDeepHealth(sessions ...ports.SessionHealthDetail) ports.DeepHealth {
	return ports.DeepHealth{Running: true, Healthy: true, Sessions: sessions}
}

// TestRolloutConvergence_DormantLeaseSessionProvesNothing is the case that made
// the confirm window useless: ready, and over nothing.
func TestRolloutConvergence_DormantLeaseSessionProvesNothing(t *testing.T) {
	ready, provable := ports.RolloutConvergence(healthyDeepHealth(leaseSession(false, false, false)))

	require.True(t, ready, "the readiness fold skips a dormant lease session, and that stays true")
	require.False(t, provable, "but nothing was observed, so it proves nothing about the new config")
}

// TestRolloutConvergence_TheLeaseHolderProvesIt is the other half: the member
// that took the lease has actually spoken to the broker.
func TestRolloutConvergence_TheLeaseHolderProvesIt(t *testing.T) {
	ready, provable := ports.RolloutConvergence(healthyDeepHealth(leaseSession(true, true, true)))

	require.True(t, ready)
	require.True(t, provable)
}

// TestRolloutConvergence_TheLeaseHolderThatCannotServeIsNotReady pins the
// verdict the window is there to act on — the broker refused something, so the
// member is not ready and never records convergence.
func TestRolloutConvergence_TheLeaseHolderThatCannotServeIsNotReady(t *testing.T) {
	ready, provable := ports.RolloutConvergence(healthyDeepHealth(leaseSession(true, true, false)))

	require.False(t, ready, "its subscriptions are not satisfied")
	require.False(t, provable)
}

// TestRolloutConvergence_NoSessionsIsProvable keeps the rule from breaking every
// deployment that has no stateful session at all: there is nothing that could
// later contradict its readiness.
func TestRolloutConvergence_NoSessionsIsProvable(t *testing.T) {
	ready, provable := ports.RolloutConvergence(healthyDeepHealth())

	require.True(t, ready)
	require.True(t, provable)
}

// TestRolloutConvergence_OneLiveSessionIsEnough covers the mixed member: a
// dormant lease session beside a session that is genuinely up still rests on
// something observed.
func TestRolloutConvergence_OneLiveSessionIsEnough(t *testing.T) {
	live := ports.SessionHealthDetail{
		SessionID:              "sqs",
		Connected:              true,
		SubscriptionsSatisfied: satisfied(true),
		ServiceLevel:           ports.ServiceLevelFull,
	}
	ready, provable := ports.RolloutConvergence(healthyDeepHealth(leaseSession(false, false, false), live))

	require.True(t, ready)
	require.True(t, provable)
}

// TestRolloutConvergence_AStoppedRuntimeConvergesNothing keeps it fail-closed.
func TestRolloutConvergence_AStoppedRuntimeConvergesNothing(t *testing.T) {
	ready, provable := ports.RolloutConvergence(ports.DeepHealth{})

	require.False(t, ready)
	require.False(t, provable)
}
