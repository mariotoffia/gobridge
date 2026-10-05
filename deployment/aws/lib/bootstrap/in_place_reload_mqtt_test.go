package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// These tests run the tracked fake as the "mqtt" transport, so the sessions
// carry real paho configs through resolution and the fake counts closes. A
// session's options depend only on its own configuration, so adding an MQTT
// session touches no other unit.

// mqttInPlaceConfig is inPlaceTestConfig with every owner's session a paho
// session.
func mqttInPlaceConfig(owners ...string) *ports.BridgeConfig {
	cfg := inPlaceTestConfig(owners...)
	for i := range cfg.Sessions {
		sessionCfg := paho.DefaultConfig()
		sessionCfg.Session.BrokerURL = "tcp://broker:1883"
		sessionCfg.Session.ClientID = cfg.Sessions[i].ID
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

func runtimeRouteIDs(app *App) []string {
	var routes []string
	for _, route := range app.CurrentRuntime().Routes() {
		routes = append(routes, route.ID)
	}
	return routes
}

func TestApplyInPlace_AddingAnMQTTSessionKeepsOtherMQTTSessionsConnected(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	require.NoError(t, applyTo(t, app, mqttInPlaceConfig("a", "b")))
	rt := app.CurrentRuntime()

	next := mqttInPlaceConfig("a", "b", "c")
	next.Version = 2
	require.NoError(t, applyTo(t, app, next))

	assert.Same(t, rt, app.CurrentRuntime(), "the reload is in place")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a's session is never closed")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b's session is never closed")
	assert.Equal(t, []int{0}, tf.closeCounts("c-s"), "only the new owner's unit is added")
	assert.ElementsMatch(t, []string{"a", "b", "c", "h"}, runtimeRouteIDs(app))
	ownerA := app.registryRef.Load().cfg.Sessions[0].Config.(*paho.Config)
	assert.Zero(t, ownerA.Session.ReceiveMaximum, "nothing derives owner a's receive_maximum")
}

func TestApplyInPlace_ReloadOverMaxMQTTSessionsKeepsRunningConfiguration(t *testing.T) {
	// Owner h rides on HTTP with no session, so the base uses exactly two MQTT
	// sessions: at the limit, not over it.
	const limit = 2
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	base := mqttInPlaceConfig("a", "b")
	base.Bridge.MaxMQTTSessions = limit
	require.NoError(t, applyTo(t, app, base))
	rt := app.CurrentRuntime()

	next := mqttInPlaceConfig("a", "b", "c")
	next.Bridge.MaxMQTTSessions = limit
	next.Version = 2
	err := applyTo(t, app, next)

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.ErrorContains(t, err, "max_mqtt_sessions")
	assert.ErrorContains(t, err, "uses 3 MQTT sessions, more than the limit of 2")
	assert.Same(t, rt, app.CurrentRuntime(), "the running runtime is kept")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a's session is never closed")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b's session is never closed")
	assert.Empty(t, tf.closeCounts("c-s"), "no session is built for the refused owner")
	assert.ElementsMatch(t, []string{"a", "b", "h"}, runtimeRouteIDs(app))
	assert.Same(t, base, app.CurrentAppliedConfig(), "the running configuration stays installed")
	assert.Equal(t, 1, app.CurrentAppliedConfig().Version)
}
