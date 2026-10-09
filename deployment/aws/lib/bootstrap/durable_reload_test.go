package bootstrap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// durableConfig is mqttInPlaceConfig with every owner's session persistent, so
// each session has a durable broker identity, and a DLQ store holding records.
func durableConfig(owners ...string) *ports.BridgeConfig {
	cfg := mqttInPlaceConfig(owners...)
	for i := range cfg.Sessions {
		cfg.Sessions[i].SessionMode = "persistent"
	}
	cfg.Stores.DLQ = &ports.StoreConfig{Type: "memory", Config: &nativestore.MemoryConfig{AcknowledgeVolatile: true}}
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

func TestApply_AcceptsAReloadThatChangesADurableBrokerIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		next  func(t *testing.T) *ports.BridgeConfig
		ended int
	}{
		"protocol switch, which keeps the broker state": {
			next: func(t *testing.T) *ports.BridgeConfig {
				cfg := durableConfig("a", "b")
				sessionOptions(t, cfg, "a-s").ProtocolVersion = paho.ProtocolVersion311
				return cfg
			},
		},
		"client_id change beside a bridge-wide change, which replaces the runtime": {
			next: func(t *testing.T) *ports.BridgeConfig {
				cfg := durableConfig("a", "b")
				sessionOptions(t, cfg, "a-s").ClientID = "a-s-renamed"
				cfg.Bridge.DrainTimeout = "2s"
				return cfg
			},
			ended: 1,
		},
		"removed durable session, which reloads in place": {
			next:  func(*testing.T) *ports.BridgeConfig { return durableConfig("b") },
			ended: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tf := newTrackedTransportFactory(true)
			app := newMQTTInPlaceTestApp(t, tf)
			require.NoError(t, applyTo(t, app, durableConfig("a", "b")))

			cfg := tc.next(t)
			cfg.Version = 2
			require.NoError(t, bridge.ValidateDurableReload(durableConfig("a", "b"), cfg))
			require.NoError(t, applyTo(t, app, cfg), "a changed broker identity is an ordinary reload (ADR 0024)")

			assert.Same(t, cfg, app.CurrentAppliedConfig())
			assert.Equal(t, tc.ended, tf.endedCount("a-s"), "only a lost broker state key is ended")
			assert.Zero(t, tf.endedCount("b-s"), "owner b keeps its broker identity")
		})
	}
}

func TestApply_RefusesAReloadThatStrandsDurableRecords(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	base := durableConfig("a", "b")
	require.NoError(t, applyTo(t, app, base))
	rt := app.CurrentRuntime()

	cfg := durableConfig("a", "b")
	cfg.Stores.DLQ = nil
	cfg.Version = 2
	refusal := bridge.ValidateDurableReload(base, cfg)
	require.Error(t, refusal, "removing the DLQ store strands its records")

	assert.Equal(t, refusal, applyTo(t, app, cfg))
	assert.Same(t, rt, app.CurrentRuntime(), "the running runtime is kept")
	assert.Same(t, base, app.CurrentAppliedConfig(), "the running configuration stays installed")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "the durable session is never closed")
	assert.Zero(t, tf.endedCount("a-s"))
}

func TestApply_DurableSessionKeepsItsIdentityThroughAUnitReload(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newMQTTInPlaceTestApp(t, tf)
	require.NoError(t, applyTo(t, app, durableConfig("a", "b")))
	rt := app.CurrentRuntime()

	require.NoError(t, applyTo(t, app, withRouteChange(durableConfig("a", "b"), "a", 2)))

	assert.Same(t, rt, app.CurrentRuntime(), "the reload is in place")
	assert.Equal(t, 2, app.CurrentAppliedConfig().Version)
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a's changed unit is rebuilt on the same identity")
}

// BrokerStateKeys keys an MQTT session's broker state as the MQTT transport
// does, so a reload asks the tracked sessions to end what it would end in
// production.
func (f *trackedTransportFactory) BrokerStateKeys(session ports.SessionSpec, receivers []ports.ReceiverSpec) ([]string, error) {
	if _, ok := session.Config.(*paho.Config); !ok {
		return nil, nil
	}
	return paho.NewFactory(nil).BrokerStateKeys(session, receivers)
}

func (s *trackedSession) EndBrokerStateOnClose(time.Time) {
	s.factory.mu.Lock()
	defer s.factory.mu.Unlock()
	if s.factory.ended == nil {
		s.factory.ended = map[string]int{}
	}
	s.factory.ended[s.id]++
}

// endedCount is how often the sessions built for id were asked to end their
// broker state.
func (f *trackedTransportFactory) endedCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended[id]
}

var (
	_ ports.BrokerStateKeyer = (*trackedTransportFactory)(nil)
	_ ports.BrokerStateEnder = (*trackedSession)(nil)
)
