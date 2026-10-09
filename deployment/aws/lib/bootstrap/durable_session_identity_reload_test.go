package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// persistentMQTTConfig is mqttInPlaceConfig with every owner's session
// persistent, so each session has a durable broker identity.
func persistentMQTTConfig(owners ...string) *ports.BridgeConfig {
	cfg := mqttInPlaceConfig(owners...)
	for i := range cfg.Sessions {
		cfg.Sessions[i].SessionMode = "persistent"
	}
	return cfg
}

func sessionOptions(t *testing.T, cfg *ports.BridgeConfig, id string) *paho.SessionOptions {
	t.Helper()
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID == id {
			return &cfg.Sessions[i].Config.(*paho.Config).Session
		}
	}
	require.FailNow(t, "no session", id)
	return nil
}

func TestApply_RefusesADurableSessionIdentityChange(t *testing.T) {
	for name, next := range map[string]func(t *testing.T) *ports.BridgeConfig{
		"protocol switch, which reloads in place": func(t *testing.T) *ports.BridgeConfig {
			cfg := persistentMQTTConfig("a", "b")
			sessionOptions(t, cfg, "a-s").ProtocolVersion = paho.ProtocolVersion311
			return cfg
		},
		"client_id change beside a bridge-wide change, which replaces the runtime": func(t *testing.T) *ports.BridgeConfig {
			cfg := persistentMQTTConfig("a", "b")
			sessionOptions(t, cfg, "a-s").ClientID = "a-s-renamed"
			cfg.Bridge.DrainTimeout = "2s"
			return cfg
		},
		"removed session": func(*testing.T) *ports.BridgeConfig { return persistentMQTTConfig("b") },
	} {
		t.Run(name, func(t *testing.T) {
			tf := newTrackedTransportFactory(true)
			app := newMQTTInPlaceTestApp(t, tf)
			base := persistentMQTTConfig("a", "b")
			require.NoError(t, applyTo(t, app, base))
			rt := app.CurrentRuntime()

			cfg := next(t)
			cfg.Version = 2
			refusal := bridge.DurableSessionIdentityChanged(base, cfg)
			require.Error(t, refusal, "the change strands durable session a-s")

			assert.Equal(t, refusal, applyTo(t, app, cfg))
			assert.Same(t, rt, app.CurrentRuntime(), "the running runtime is kept")
			assert.Same(t, base, app.CurrentAppliedConfig(), "the running configuration stays installed")
			assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "the durable session is never closed")
		})
	}
}

func TestApply_DurableSessionKeepsItsIdentityThroughAUnitReload(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	require.NoError(t, applyTo(t, app, persistentMQTTConfig("a", "b")))
	rt := app.CurrentRuntime()

	require.NoError(t, applyTo(t, app, withRouteChange(persistentMQTTConfig("a", "b"), "a", 2)))

	assert.Same(t, rt, app.CurrentRuntime(), "the reload is in place")
	assert.Equal(t, 2, app.CurrentAppliedConfig().Version)
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a's changed unit is rebuilt on the same identity")
}
