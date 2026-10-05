package bootstrap

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/ports"
)

// defaultReceiveMaximumDocument is one MQTT session that leaves
// receive_maximum unset under a 256 KiB payload ceiling, with one route that
// admits 32 deliveries in flight.
const defaultReceiveMaximumDocument = `
bridge:
  id: mqtt-default-receive-maximum
  deployment_mode: standalone
  drain_timeout: 1s
sessions:
  - id: ingress
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
        max_payload_bytes: 262144
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: telemetry/in
        qos: 1
senders:
  - id: sender
    transport: mqtt
    session_id: ingress
bindings:
  - id: out
    sender_id: sender
    address: telemetry/out
routes:
  - id: route
    receiver_id: receiver
    delivery_mode: direct_hold
    bindings: [out]
    policy:
      max_in_flight: 32
      on_permanent_failure: drop
      on_expired: drop
`

func decodeDefaultReceiveMaximumDocument(t *testing.T, app *App) *ports.BridgeConfig {
	t.Helper()
	cfg, err := app.decodeInitialConfig(strings.NewReader(defaultReceiveMaximumDocument))
	require.NoError(t, err)
	return cfg
}

func TestMQTTSessionWithDefaultReceiveMaximum_PlanAccepts(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	cfg := decodeDefaultReceiveMaximumDocument(t, app)
	require.IsType(t, &paho.Config{}, cfg.Sessions[0].Config)

	// The App's registry carries the real paho factory. Plan opens no session,
	// so no broker is dialled.
	plan, err := app.newFactoryRegistry(cfg).builder.Plan(t.Context())
	require.NoError(t, err)
	plan.Abort()
}

func TestMQTTSessionWithDefaultReceiveMaximum_AWSAppRunsReceiveMaximum192(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	var (
		mu    sync.Mutex
		specs []ports.SessionSpec
	)
	tf.onSessionSpec = func(spec ports.SessionSpec) {
		mu.Lock()
		defer mu.Unlock()
		specs = append(specs, spec)
	}
	app := newMQTTInPlaceTestApp(t, tf)

	require.NoError(t, applyTo(t, app, decodeDefaultReceiveMaximumDocument(t, app)))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, specs, 1)
	require.Equal(t, "ingress", specs[0].ID)
	built, ok := specs[0].Config.(*paho.Config)
	require.True(t, ok, "the factory receives a paho config, got %T", specs[0].Config)
	assert.Zero(t, built.Session.ReceiveMaximum, "nothing derives receive_maximum")
	assert.Equal(t, uint32(262144), built.Session.MaxPayloadBytes)

	created, err := paho.NewFactory(nil).NewSession(t.Context(), specs[0])
	require.NoError(t, err)
	session, ok := created.(*paho.Session)
	require.True(t, ok, "the paho factory returns a paho session, got %T", created)
	_, capacity := session.DispatchStats()
	assert.Equal(t, 192, capacity)
}
