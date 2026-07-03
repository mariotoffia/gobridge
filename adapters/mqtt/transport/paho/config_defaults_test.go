package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Registry-path defaults: the decoder decodes into a DefaultConfig()
// pre-filled value, so every documented default applies on the typed
// YAML path, while explicit values — INCLUDING explicit zeros — win.
// Field types cannot become pointers (out-of-module tests build
// SessionOptions/SenderOptions literals), so prefill is the mechanism
// that distinguishes "unset" from "explicit zero".
// ═══════════════════════════════════════════════════════════════════════════

func decodeRegistry(t *testing.T, input map[string]any) *Config {
	t.Helper()
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	pc, err := reg.Decode("mqtt", parser.NewRawConfig(input))
	require.NoError(t, err)
	cfg, ok := pc.(*Config)
	require.True(t, ok)
	return cfg
}

// TestRegistryDecode_DefaultsApplied verifies every documented default
// materialises when the YAML omits the keys (the keep_alive-default
// defect: zero-value decode disabled the MQTT pinger silently).
func TestRegistryDecode_DefaultsApplied(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"client_id":   "c1",
			"broker_urls": []any{"tcp://b:1883"},
		},
	})

	require.Equal(t, uint16(30), cfg.Session.KeepAlive,
		"keep_alive default must apply on the typed YAML path (0 disables the pinger)")
	require.Equal(t, 30*time.Second, cfg.Session.ConnectTimeout)
	require.Equal(t, 30*time.Second, cfg.Session.ReconnectTimeout)
	require.False(t, cfg.Session.CleanStart, "clean_start defaults to false")
	require.Nil(t, cfg.Session.Will, "no will unless configured")

	require.Equal(t, byte(1), cfg.Sender.QoS, "sender qos defaults to 1")
	require.Equal(t, 30*time.Second, cfg.Sender.Timeout)
	require.Equal(t, 500*time.Millisecond, cfg.Sender.ThrottleRetryAfter)
}

// TestRegistryDecode_ExplicitZerosHonoured verifies an EXPLICIT zero is
// not clobbered by the defaults: qos: 0 stays at-most-once and
// keep_alive: 0 disables the pinger deliberately.
func TestRegistryDecode_ExplicitZerosHonoured(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"client_id":   "c1",
			"broker_urls": []any{"tcp://b:1883"},
			"keep_alive":  0,
		},
		"sender": map[string]any{
			"default_topic": "out",
			"qos":           0,
		},
	})

	require.Equal(t, uint16(0), cfg.Session.KeepAlive,
		"explicit keep_alive: 0 must be honoured")
	require.Equal(t, byte(0), cfg.Sender.QoS,
		"explicit qos: 0 must be honoured (the factory previously coerced it to 1)")
}

// TestFactoryNewSender_ExplicitQoS0Honoured pins the factory half of
// the qos-0 fix: an explicit at-most-once sender stays QoS 0.
func TestFactoryNewSender_ExplicitQoS0Honoured(t *testing.T) {
	f := NewFactory(nil)
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "qos0-sender",
	}, connectivity.SessionEphemeral, nil)

	snd, err := f.NewSender(t.Context(), ports.SenderSpec{
		ID:     "s1",
		Config: &Config{Sender: SenderOptions{DefaultTopic: "out", QoS: 0}},
	}, sess)
	require.NoError(t, err)
	require.Equal(t, byte(0), snd.(*Sender).opts.QoS,
		"explicit QoS 0 must survive the factory")
}

// TestRegistryDecode_CanonicalNestedYAMLShape is the pin for the
// canonical `options:` shape: SESSION and SENDER settings are NESTED
// under `session:` / `sender:` keys. The flat shape shown in older docs
// (broker_url/client_id directly under options:) is rejected by the
// strict decoder — asserted here so the docs stay honest.
func TestRegistryDecode_CanonicalNestedYAMLShape(t *testing.T) {
	const canonical = `
session:
  broker_url: tcp://localhost:1883
  client_id: bridge-01
  keep_alive: 60
  clean_start: false
  session_expiry_interval: 3600
  will:
    topic: bridge/status/bridge-01
    payload: offline
    qos: 1
    retain: true
sender:
  default_topic: out/topic
  qos: 1
`
	var input map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(canonical), &input))

	cfg := decodeRegistry(t, input)
	require.Equal(t, []string{"tcp://localhost:1883"}, cfg.Session.BrokerURLs)
	require.Equal(t, "bridge-01", cfg.Session.ClientID)
	require.Equal(t, uint16(60), cfg.Session.KeepAlive)
	require.Equal(t, uint32(3600), cfg.Session.SessionExpiryInterval)
	require.NotNil(t, cfg.Session.Will)
	require.Equal(t, "bridge/status/bridge-01", cfg.Session.Will.Topic)
	require.Equal(t, "offline", cfg.Session.Will.Payload)
	require.Equal(t, byte(1), cfg.Session.Will.QoS)
	require.True(t, cfg.Session.Will.Retain)
	require.Equal(t, "out/topic", cfg.Sender.DefaultTopic)

	// The FLAT shape from the old docs must fail strict decoding.
	const flat = `
broker_url: tcp://localhost:1883
client_id: bridge-01
`
	var flatInput map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(flat), &flatInput))
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	_, err := reg.Decode("mqtt", parser.NewRawConfig(flatInput))
	require.Error(t, err, "flat option keys are not the canonical shape and must be rejected")
	require.Contains(t, err.Error(), "invalid keys")
}

// TestRegistryDecode_WillValidation verifies invalid will configs fail
// the registry decode with a clear error.
func TestRegistryDecode_WillValidation(t *testing.T) {
	cases := []struct {
		name string
		will map[string]any
		want string
	}{
		{"missing topic", map[string]any{"payload": "x"}, "will.topic is required"},
		{"wildcard topic", map[string]any{"topic": "a/+/b"}, "must not contain wildcards"},
		{"bad qos", map[string]any{"topic": "a/b", "qos": 3}, "will.qos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := ports.NewRegistry()
			require.NoError(t, Register(reg))
			_, err := reg.Decode("mqtt", parser.NewRawConfig(map[string]any{
				"session": map[string]any{
					"client_id":   "c1",
					"broker_urls": []any{"tcp://b:1883"},
					"will":        tc.will,
				},
			}))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestSessionOptionsFromMap_Will covers the legacy map path for will
// options (map form used by hand-assembled option maps).
func TestSessionOptionsFromMap_Will(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"broker_urls": []string{"tcp://b:1883"},
		"client_id":   "c1",
		"will": map[string]any{
			"topic":   "status/c1",
			"payload": "offline",
			"qos":     1,
			"retain":  true,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, opts.Will)
	require.Equal(t, "status/c1", opts.Will.Topic)
	require.Equal(t, "offline", opts.Will.Payload)
	require.Equal(t, byte(1), opts.Will.QoS)
	require.True(t, opts.Will.Retain)
}
