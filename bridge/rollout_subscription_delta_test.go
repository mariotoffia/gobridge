package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// Whether a coordinated cohort may change a receiver's subscription list while
// it is running, and what happens when it may not.
//
// A durable MQTT session survives the swap, so a subscription the cohort removes
// keeps being delivered unless something remembers that the adapter installed it.
// That memory is the managed-subscription store (ADR 0003): with it, a reconcile
// converges both directions against the exact filters this identity applied;
// without it, a persistent or exclusive MQTT session that wants subscriptions
// cannot be BUILT at all.
//
// Both halves matter to the barrier and are pinned here. The change is live-safe,
// so it travels through the barrier rather than demanding a whole-cohort
// replacement — and a member that cannot build it says so with a Nack, which
// aborts the rollout at the vote instead of letting it burn its deadline while
// the cohort waits for an answer that is never coming.

// freezableManagedIdentityConfig is managedIdentityConfig plus the adapter-owned
// freeze capability every durable session config carries in production. The
// classifier freezes both sides of a delta before comparing their identities, so
// a fixture without it cannot be classified at all.
type freezableManagedIdentityConfig struct{ managedIdentityConfig }

func (c freezableManagedIdentityConfig) FreezePluginConfig() ports.PluginConfig { return c }

var _ ports.FreezableConfig = freezableManagedIdentityConfig{}

func managedSubscriptionCohortConfig(topics ...string) *ports.BridgeConfig {
	subs := make([]ports.SubscriptionDef, 0, len(topics))
	for _, topic := range topics {
		subs = append(subs, ports.SubscriptionDef{Topic: topic, QoS: 1})
	}
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "cohort",
			DeploymentMode: "clustered",
			Cluster: &ports.ClusterConfig{
				Rollout: "coordinated",
				Members: []string{"a", "b"},
			},
		},
		Stores: ports.StoresConfig{
			ManagedSubscriptions: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent", Config: freezableManagedIdentityConfig{}},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", SessionID: "mqtt-sess", Topics: subs},
		},
	}
}

// TestClassifyRolloutDelta_SubscriptionChangeIsLiveSafeOnAManagedSession records
// the decision: a managed session's whole purpose is that its subscription set
// can move, so changing it does not demand a whole-cohort replacement.
func TestClassifyRolloutDelta_SubscriptionChangeIsLiveSafeOnAManagedSession(t *testing.T) {
	for name, delta := range map[string][2]*ports.BridgeConfig{
		"adding a subscription": {
			managedSubscriptionCohortConfig("sensors/a"),
			managedSubscriptionCohortConfig("sensors/a", "sensors/b"),
		},
		"removing a subscription": {
			managedSubscriptionCohortConfig("sensors/a", "sensors/b"),
			managedSubscriptionCohortConfig("sensors/a"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			class, reason := classifyRolloutDelta(delta[0], delta[1])
			require.Equal(t, rolloutLiveSafe, class, "reason: %s", reason)
			require.Empty(t, reason)
		})
	}
}

// TestClassifyRolloutDelta_ManagedSubscriptionStoreChangeStillNeedsReplacement is
// the boundary: the subscriptions may move, but the store that remembers them is
// a durable identity and cannot.
func TestClassifyRolloutDelta_ManagedSubscriptionStoreChangeStillNeedsReplacement(t *testing.T) {
	before := managedSubscriptionCohortConfig("sensors/a")
	after := managedSubscriptionCohortConfig("sensors/a")
	after.Stores.ManagedSubscriptions = &ports.StoreConfig{Type: "sqlite"}

	class, reason := classifyRolloutDelta(before, after)
	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason)
}

// TestSubscriptionChangeWithoutAManagedStoreIsNackedNotWaitedOut pins the other
// half. A durable MQTT session that wants subscriptions and has no store to
// remember them is refused by the candidate build, and that refusal must reach
// the barrier as a Nack — the coordinator aborts on the first one, so the cohort
// learns immediately instead of waiting out the rollout deadline for a vote that
// was never going to arrive.
func TestSubscriptionChangeWithoutAManagedStoreIsNackedNotWaitedOut(t *testing.T) {
	cfg := managedSubscriptionCohortConfig("sensors/a", "sensors/b")
	cfg.Stores.ManagedSubscriptions = nil

	_, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, nil)
	require.Error(t, err, "a durable MQTT session with subscriptions and no store cannot be built")
	require.Contains(t, err.Error(), "stores.managed_subscriptions",
		"the refusal must name the store the operator has to wire")
	require.False(t, transientBuildFailure(context.Background(), err),
		"this refusal is a property of the candidate, so it must cast the permanent Nack "+
			"that aborts the rollout rather than abstaining and letting the deadline decide")
	require.False(t, transientBuildFailure(context.Background(), errors.Join(err)),
		"a wrapped refusal is still permanent")
}
