package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
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
      queue_url: https://sqs.us-west-1.amazonaws.com/123456789/orders

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
	cfg, err := Parse(strings.NewReader(input), FormatYAML, passthroughRegistry("dynamodb", "mqtt", "sqs"))
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
	cfg, err := Parse(strings.NewReader(input), FormatJSON, passthroughRegistry("sqs"))
	require.NoError(t, err)
	assert.Equal(t, "bridge-json", cfg.Bridge.ID)
	require.Len(t, cfg.Routes, 1)
}

// Verifies Parse returns an error for malformed YAML input.
func TestParse_InvalidYAML(t *testing.T) {
	_, err := Parse(strings.NewReader("{{invalid"), FormatYAML, passthroughRegistry())
	assert.Error(t, err)
}

// Verifies Parse returns an error for malformed JSON input.
func TestParse_InvalidJSON(t *testing.T) {
	_, err := Parse(strings.NewReader("{not json"), FormatJSON, passthroughRegistry())
	assert.Error(t, err)
}

// Verifies detectFormat maps common file extensions to YAML or JSON, defaulting unknown extensions to YAML.
func TestDetectFormat(t *testing.T) {
	assert.Equal(t, FormatYAML, detectFormat("config.yaml"))
	assert.Equal(t, FormatYAML, detectFormat("config.yml"))
	assert.Equal(t, FormatJSON, detectFormat("config.json"))
	assert.Equal(t, FormatYAML, detectFormat("config.txt"))
}

// Verifies ports.BridgeSettings duration helpers parse explicit shutdown and drain timeout strings to nanoseconds.
func TestBridgeSettings_Durations(t *testing.T) {
	bs := ports.BridgeSettings{ShutdownTimeout: "10s", DrainTimeout: "5s"}
	assert.Equal(t, 10*1e9, float64(bs.ShutdownTimeoutDuration()))
	assert.Equal(t, 5*1e9, float64(bs.DrainTimeoutDuration()))
}

// Verifies ports.BridgeSettings duration helpers apply the default 30s when shutdown and drain timeouts are unset.
func TestBridgeSettings_DurationDefaults(t *testing.T) {
	bs := ports.BridgeSettings{}
	assert.Equal(t, 30*1e9, float64(bs.ShutdownTimeoutDuration()))
	assert.Equal(t, 30*1e9, float64(bs.DrainTimeoutDuration()))
}

// TestParseFile_ValidYAML verifies ParseFile reads a YAML temp file with FormatAuto
// and correctly populates the bridge config fields.
func TestParseFile_ValidYAML(t *testing.T) {
	content := []byte(`
bridge:
  id: file-test
receivers:
  - id: rx-file
    transport: sqs
routes:
  - id: r-file
    receiver_id: rx-file
    bindings: []
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := ParseFile(path, FormatAuto, passthroughRegistry("sqs"))
	require.NoError(t, err)
	assert.Equal(t, "file-test", cfg.Bridge.ID)
	require.Len(t, cfg.Receivers, 1)
	assert.Equal(t, "rx-file", cfg.Receivers[0].ID)
}

// TestParseFile_ValidJSON verifies ParseFile detects JSON format from a .json extension
// and parses the content correctly.
func TestParseFile_ValidJSON(t *testing.T) {
	content := []byte(`{
  "bridge": {"id": "json-file"},
  "receivers": [{"id": "rx-json", "transport": "sqs"}],
  "routes": [{"id": "r-json", "receiver_id": "rx-json", "bindings": []}]
}`)
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := ParseFile(path, FormatAuto, passthroughRegistry("sqs"))
	require.NoError(t, err)
	assert.Equal(t, "json-file", cfg.Bridge.ID)
	require.Len(t, cfg.Receivers, 1)
	assert.Equal(t, "rx-json", cfg.Receivers[0].ID)
}

// TestParseFile_NonExistentFile verifies ParseFile returns an error whose message
// contains "config: open" when the file does not exist.
func TestParseFile_NonExistentFile(t *testing.T) {
	_, err := ParseFile("/tmp/gobridge-nonexistent-12345.yaml", FormatAuto, passthroughRegistry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config: open")
}

// TestParseFile_FormatOverride verifies that an explicit format parameter takes
// precedence over the file extension — JSON content in a .yaml file parses when
// FormatJSON is specified.
func TestParseFile_FormatOverride(t *testing.T) {
	content := []byte(`{"bridge": {"id": "override-test"}}`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := ParseFile(path, FormatJSON, passthroughRegistry())
	require.NoError(t, err)
	assert.Equal(t, "override-test", cfg.Bridge.ID)
}

// TestParse_UnsupportedFormat verifies Parse returns an error containing
// "unsupported format" for an unrecognized format string.
func TestParse_UnsupportedFormat(t *testing.T) {
	_, err := Parse(strings.NewReader("<xml/>"), Format("xml"), passthroughRegistry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")
}

// TestParse_EmptyInput verifies Parse does not error on an empty reader and
// returns a valid zero-valued config.
func TestParse_EmptyInput(t *testing.T) {
	cfg, err := Parse(strings.NewReader(""), FormatYAML, passthroughRegistry())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Bridge.ID)
	assert.Empty(t, cfg.Routes)
}

// TestParse_OversizedInput_RejectedByLimit verifies that Parse rejects inputs
// exceeding MaxConfigBytes to prevent DoS via oversized configuration.
func TestParse_OversizedInput_RejectedByLimit(t *testing.T) {
	data := strings.Repeat("a: b\n", MaxConfigBytes/5+1)
	require.Greater(t, len(data), MaxConfigBytes)

	_, err := Parse(strings.NewReader(data), FormatYAML, passthroughRegistry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

// TestDetectFormat_CaseInsensitive verifies that detectFormat handles upper-case
// and mixed-case file extensions via strings.ToLower in the implementation.
func TestDetectFormat_CaseInsensitive(t *testing.T) {
	tests := []struct {
		path string
		want Format
	}{
		{"config.JSON", FormatJSON},
		{"config.YAML", FormatYAML},
		{"config.YML", FormatYAML},
		{"config.Json", FormatJSON},
		{"config.Yml", FormatYAML},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, detectFormat(tt.path))
		})
	}
}
