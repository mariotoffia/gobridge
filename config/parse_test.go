package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Verifies Parse unmarshals a representative YAML bridge config into the expected struct fields.
func TestParse_YAML(t *testing.T) {
	input := `
bridge:
  id: bridge-1
  shutdown_timeout: 30s

stores:
  lease:
    type: dynamodb
    options:
      table_name: gobridge-leases
  outbox:
    type: dynamodb
    options:
      table_name: gobridge-outbox

sessions:
  - id: mqtt-sess
    transport: mqtt
    session_mode: exclusive
    options:
      client_id: factory-a
      broker_urls:
        - ssl://broker:8883

receivers:
  - id: sqs-rx
    transport: sqs
    options:
      queue_url: https://sqs.us-east-1.amazonaws.com/123456789/orders

senders:
  - id: mqtt-tx
    transport: mqtt
    session_id: mqtt-sess

bindings:
  - id: bind-a
    sender_id: mqtt-tx
    session_id: mqtt-sess
    address: "factory/a/orders/{device_id}"
    options:
      qos: 1

routes:
  - id: sqs-to-mqtt
    receiver_id: sqs-rx
    delivery_mode: shared_outbox
    dispatch_mode: single
    policy:
      max_in_flight: 50
      ack_after: outbox_persist
    bindings:
      - bind-a
    session:
      session_id: mqtt-sess
      sender_id: mqtt-tx
      lease_ttl: 30s
      drain_interval: 1s
`
	cfg, err := Parse(strings.NewReader(input), FormatYAML)
	require.NoError(t, err)

	assert.Equal(t, "bridge-1", cfg.Bridge.ID)
	assert.Equal(t, "30s", cfg.Bridge.ShutdownTimeout)

	require.NotNil(t, cfg.Stores.Lease)
	assert.Equal(t, "dynamodb", cfg.Stores.Lease.Type)

	require.NotNil(t, cfg.Stores.Outbox)
	assert.Equal(t, "dynamodb", cfg.Stores.Outbox.Type)

	require.Len(t, cfg.Sessions, 1)
	assert.Equal(t, "mqtt-sess", cfg.Sessions[0].ID)
	assert.Equal(t, "mqtt", cfg.Sessions[0].Transport)
	assert.Equal(t, "exclusive", cfg.Sessions[0].SessionMode)

	require.Len(t, cfg.Receivers, 1)
	assert.Equal(t, "sqs-rx", cfg.Receivers[0].ID)

	require.Len(t, cfg.Senders, 1)
	assert.Equal(t, "mqtt-tx", cfg.Senders[0].ID)
	assert.Equal(t, "mqtt-sess", cfg.Senders[0].SessionID)

	require.Len(t, cfg.Bindings, 1)
	assert.Equal(t, "bind-a", cfg.Bindings[0].ID)
	assert.Equal(t, "factory/a/orders/{device_id}", cfg.Bindings[0].Address)

	require.Len(t, cfg.Routes, 1)
	r := cfg.Routes[0]
	assert.Equal(t, "sqs-to-mqtt", r.ID)
	assert.Equal(t, "sqs-rx", r.ReceiverID)
	assert.Equal(t, "shared_outbox", r.DeliveryMode)
	assert.Equal(t, "single", r.DispatchMode)
	assert.Equal(t, 50, r.Policy.MaxInFlight)
	assert.Equal(t, "outbox_persist", r.Policy.AckAfter)
	require.NotNil(t, r.Session)
	assert.Equal(t, "mqtt-sess", r.Session.SessionID)
	assert.Equal(t, "mqtt-tx", r.Session.SenderID)
}

// Verifies Parse unmarshals a minimal JSON bridge config without error.
func TestParse_JSON(t *testing.T) {
	input := `{
  "bridge": {"id": "bridge-json"},
  "receivers": [{"id": "rx1", "transport": "sqs"}],
  "senders": [{"id": "tx1", "transport": "sqs"}],
  "bindings": [{"id": "b1", "sender_id": "tx1", "address": "queue://test"}],
  "routes": [{"id": "r1", "receiver_id": "rx1", "bindings": ["b1"]}]
}`
	cfg, err := Parse(strings.NewReader(input), FormatJSON)
	require.NoError(t, err)
	assert.Equal(t, "bridge-json", cfg.Bridge.ID)
	require.Len(t, cfg.Routes, 1)
}

// Verifies Parse returns an error for malformed YAML input.
func TestParse_InvalidYAML(t *testing.T) {
	_, err := Parse(strings.NewReader("{{invalid"), FormatYAML)
	assert.Error(t, err)
}

// Verifies Parse returns an error for malformed JSON input.
func TestParse_InvalidJSON(t *testing.T) {
	_, err := Parse(strings.NewReader("{not json"), FormatJSON)
	assert.Error(t, err)
}

// Verifies detectFormat maps common file extensions to YAML or JSON, defaulting unknown extensions to YAML.
func TestDetectFormat(t *testing.T) {
	assert.Equal(t, FormatYAML, detectFormat("config.yaml"))
	assert.Equal(t, FormatYAML, detectFormat("config.yml"))
	assert.Equal(t, FormatJSON, detectFormat("config.json"))
	assert.Equal(t, FormatYAML, detectFormat("config.txt"))
}

// Verifies BridgeSettings duration helpers parse explicit shutdown and drain timeout strings to nanoseconds.
func TestBridgeSettings_Durations(t *testing.T) {
	bs := BridgeSettings{ShutdownTimeout: "10s", DrainTimeout: "5s"}
	assert.Equal(t, 10*1e9, float64(bs.ShutdownTimeoutDuration()))
	assert.Equal(t, 5*1e9, float64(bs.DrainTimeoutDuration()))
}

// Verifies BridgeSettings duration helpers apply the default 30s when shutdown and drain timeouts are unset.
func TestBridgeSettings_DurationDefaults(t *testing.T) {
	bs := BridgeSettings{}
	assert.Equal(t, 30*1e9, float64(bs.ShutdownTimeoutDuration()))
	assert.Equal(t, 30*1e9, float64(bs.DrainTimeoutDuration()))
}
