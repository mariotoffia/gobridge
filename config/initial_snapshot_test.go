package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestInitialSnapshotOwnsNestedBlueprintState(t *testing.T) {
	cfg := minimalValidConfig("original")
	cfg.Bridge.Cluster = &ports.ClusterConfig{Endpoints: map[string]string{"node": "original"}}
	cfg.HTTP = &ports.HTTPConfig{AdminAddr: "original"}
	cfg.ConfigWatch = &ports.ConfigWatchDef{Mode: "poll"}
	plugin := &initialPlugin{Values: map[string]string{"option": "original"}}
	cfg.Stores.Lease = &ports.StoreConfig{Type: "mqtt", Config: plugin}
	cfg.Receivers[0].Topics = []ports.SubscriptionDef{{Topic: "original", Config: plugin}}
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		HeaderMap: map[string]string{"key": "original"},
		Rules: []ports.RuleDef{{Match: []ports.ConditionDef{{Value: map[string]any{
			"nested": []any{map[string]string{"key": "original"}},
		}}}}},
	}
	connect := true
	cfg.Routes[0].Session = &ports.RouteSessionDef{ConnectAfterLease: &connect, DrainStrategy: &ports.DrainStrategyDef{}}
	out, err := initialSnapshot(cfg)
	require.NoError(t, err)
	out.Bridge.Cluster.Endpoints["node"] = "changed"
	out.HTTP.AdminAddr = "changed"
	out.ConfigWatch.Mode = "changed"
	out.Stores.Lease.Config.(*initialPlugin).Values["option"] = "changed"
	out.Receivers[0].Topics[0].Topic = "changed"
	out.Routes[0].Resolver.HeaderMap["key"] = "changed"
	out.Routes[0].Resolver.Rules[0].Match[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]string)["key"] = "changed"
	*out.Routes[0].Session.ConnectAfterLease = false
	require.Equal(t, "original", cfg.Bridge.Cluster.Endpoints["node"])
	require.Equal(t, "original", cfg.HTTP.AdminAddr)
	require.Equal(t, "poll", cfg.ConfigWatch.Mode)
	require.Equal(t, "original", plugin.Values["option"])
	require.Equal(t, "original", cfg.Receivers[0].Topics[0].Topic)
	require.Equal(t, "original", cfg.Routes[0].Resolver.HeaderMap["key"])
	require.Equal(t, "original", cfg.Routes[0].Resolver.Rules[0].Match[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]string)["key"])
	require.True(t, connect)
	require.NotSame(t, cfg.Routes[0].Session.DrainStrategy, out.Routes[0].Session.DrainStrategy)
}

func TestInitialSnapshotRejectsUnownedPluginAndConditionState(t *testing.T) {
	cfg := minimalValidConfig("original")
	cfg.Receivers[0].Config = struct{ ports.PluginConfig }{&initialPlugin{}}
	_, err := initialSnapshot(cfg)
	require.ErrorIs(t, err, shared.ErrNotSupported)
	cfg.Receivers[0].Config = (*initialPlugin)(nil)
	_, err = initialSnapshot(cfg)
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
	_, err = cloneInitialValue(make(chan string))
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}
