package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/ports"
)

// The MQTT memory profile shares one allocation — a quarter of the default
// 1 GiB container — equally among the MQTT ingress sessions, so a third owner
// takes every session's share from 128 MiB down to about 85 MiB. These tests
// run the tracked fake as the "mqtt" transport, so the sessions carry real
// paho configs through resolution and the profile, and the fake counts closes.

// mqttInPlaceConfig is inPlaceTestConfig with every owner's session a paho
// session whose ingress_memory_budget_bytes is pinned, or left to the memory
// profile when pinned is zero.
func mqttInPlaceConfig(pinned uint64, owners ...string) *ports.BridgeConfig {
	cfg := inPlaceTestConfig(owners...)
	for i := range cfg.Sessions {
		sessionCfg := paho.DefaultConfig()
		sessionCfg.Session.BrokerURL = "tcp://broker:1883"
		sessionCfg.Session.ClientID = cfg.Sessions[i].ID
		sessionCfg.Session.IngressMemoryBudgetBytes = pinned
		cfg.Sessions[i].Transport = paho.ShortKind
		cfg.Sessions[i].Config = &sessionCfg
	}
	return cfg
}

func newMQTTInPlaceTestApp(t *testing.T, tf ports.TransportFactory) *App {
	t.Helper()
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	app.extraTransports[paho.ShortKind] = tf
	return app
}

func TestApplyInPlace_AddingAnMQTTSessionRetiresEveryUnpinnedMQTTUnit(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	require.NoError(t, applyTo(t, app, mqttInPlaceConfig(0, "a", "b")))
	rt := app.CurrentRuntime()
	ownerABudget := func() uint64 {
		return app.registryRef.Load().cfg.Sessions[0].Config.(*paho.Config).Session.IngressMemoryBudgetBytes
	}
	require.Equal(t, uint64(128<<20), ownerABudget())

	next := mqttInPlaceConfig(0, "a", "b", "c")
	next.Version = 2
	require.NoError(t, applyTo(t, app, next))

	assert.Equal(t, uint64(256<<20)/3, ownerABudget(), "the profile hands owner a a third of the allocation")
	assert.Same(t, rt, app.CurrentRuntime(), "the reload is still in place")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a's share shrank, so its unit is replaced")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "owner b's share shrank, so its unit is replaced")
	assert.Equal(t, []int{0}, tf.closeCounts("c-s"))
}

func TestApplyInPlace_PinnedMQTTBudgetKeepsOtherOwnersRunning(t *testing.T) {
	const pinned = 64 << 20 // below the smallest share any of these configs hands out
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	require.NoError(t, applyTo(t, app, mqttInPlaceConfig(pinned, "a", "b")))
	rt := app.CurrentRuntime()

	next := mqttInPlaceConfig(pinned, "a", "b", "c")
	next.Version = 2
	require.NoError(t, applyTo(t, app, next))

	assert.Same(t, rt, app.CurrentRuntime())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "a pinned budget does not move with the session count")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "a pinned budget does not move with the session count")
	assert.Equal(t, []int{0}, tf.closeCounts("c-s"), "only the new owner's unit is added")
	var routes []string
	for _, route := range rt.Routes() {
		routes = append(routes, route.ID)
	}
	assert.ElementsMatch(t, []string{"a", "b", "c", "h"}, routes)
}
