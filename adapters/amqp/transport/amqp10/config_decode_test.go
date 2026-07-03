package amqp10

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	yaml "gopkg.in/yaml.v3"
)

// These tests decode through the REAL production plugin-options decoder
// (parser.NewRawConfig(...).Decode) — the exact path the runtime registry
// uses to decode a transport's `options:` block into its typed Config
// (TagName "json", ErrorUnused, and the full hook chain incl.
// floatToIntegerOrDuration + the TextUnmarshaler hook for shared.Secret).
// Importing config/parser from an adapter test is allowed: go-arch-lint
// excludes *_test.go, and there is precedent (native/config/file).

// TestPluginOptionsDecode_Nested_Succeeds is the regression test for the
// CONFIG-DECODE bug: documented multi-word snake_case option keys nested
// under session must decode into the typed Config. Before the
// mapstructure/json tags were added session.connect_timeout (and siblings)
// failed under ErrorUnused, leaving the transport unconfigurable via YAML.
func TestPluginOptionsDecode_Nested_Succeeds(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"address":            "amqp://localhost:5672",
			"connect_timeout":    "10s",
			"idle_timeout":       "90s",
			"link_close_timeout": "3s",
			"max_frame_size":     131072,
			"container_id":       "cid-1",
			"username":           "u",
			"password":           "p",
			"tls": map[string]any{
				"enable":               true,
				"ca_cert_file":         "/ca.pem",
				"insecure_skip_verify": true,
			},
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))

	require.Equal(t, "amqp://localhost:5672", cfg.Session.Address)
	require.Equal(t, 10*time.Second, cfg.Session.ConnectTimeout)
	require.Equal(t, 90*time.Second, cfg.Session.IdleTimeout)
	require.Equal(t, 3*time.Second, cfg.Session.LinkCloseTimeout)
	require.Equal(t, uint32(131072), cfg.Session.MaxFrameSize)
	require.Equal(t, "cid-1", cfg.Session.ContainerID)
	require.Equal(t, "u", cfg.Session.Username)
	// Secret decodes from a scalar string via the production TextUnmarshaler
	// hook; the real value is reachable only through Reveal.
	require.Equal(t, "p", cfg.Session.Password.Reveal())

	require.NotNil(t, cfg.Session.TLS)
	require.True(t, cfg.Session.TLS.Enable)
	require.Equal(t, "/ca.pem", cfg.Session.TLS.CACertFile)
	require.True(t, cfg.Session.TLS.InsecureSkipVerify)
}

// TestPluginOptionsDecode_UnknownKey_Errors proves the strict (ErrorUnused)
// contract is intact after tagging: an undocumented nested key still fails
// the whole decode through the production decoder.
func TestPluginOptionsDecode_UnknownKey_Errors(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"address":   "amqp://localhost:5672",
			"bogus_key": "nope",
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

// TestPluginOptionsDecode_RoutingStringForms proves the typed config
// accepts the documented string forms ("anycast"/"multicast") for the
// routing key via RoutingType's TextUnmarshaler, while remaining
// backward compatible with the original integer encoding (0/1).
func TestPluginOptionsDecode_RoutingStringForms(t *testing.T) {
	tests := []struct {
		name    string
		routing any
		want    RoutingType
		wantErr bool
	}{
		{name: "string_multicast", routing: "multicast", want: RoutingMulticast},
		{name: "string_anycast", routing: "anycast", want: RoutingAnycast},
		{name: "string_mixed_case", routing: "Multicast", want: RoutingMulticast},
		{name: "int_zero", routing: 0, want: RoutingAnycast},
		{name: "int_one", routing: 1, want: RoutingMulticast},
		{name: "invalid_string", routing: "broadcast", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{
				"receiver": map[string]any{
					"address": "orders",
					"routing": tc.routing,
				},
			}
			var cfg Config
			err := parser.NewRawConfig(input).Decode(&cfg)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Receiver.Routing)
		})
	}
}

// TestPluginOptionsDecode_CanonicalYAML pins the CANONICAL documented
// YAML shape for the amqp10 transport: role configs nested under
// session/receiver/sender (never flat keys). The YAML text below is the
// docs' reference example — if this test fails, either the decoder or
// the documentation contract changed.
func TestPluginOptionsDecode_CanonicalYAML(t *testing.T) {
	const canonical = `
session:
  address: "amqps://broker.example.com:5671"
  container_id: "gobridge-replica-1"
  connect_timeout: 10s
  sasl_mechanism: plain
  username: bridge
  password: secret
  tls:
    enable: true
receiver:
  address: "orders"
  link_credit: 50
  durability_mode: 2
  routing: multicast
  subscription_name: "orders-sub"
sender:
  address: "orders-out"
  timeout: 15s
  routing: anycast
  durable: false
credentials_uri: "aws-secretsmanager://prod/amqp"
`
	var raw map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(canonical), &raw))

	var cfg Config
	require.NoError(t, parser.NewRawConfig(raw).Decode(&cfg))

	require.Equal(t, "amqps://broker.example.com:5671", cfg.Session.Address)
	require.Equal(t, "gobridge-replica-1", cfg.Session.ContainerID)
	require.Equal(t, 10*time.Second, cfg.Session.ConnectTimeout)
	require.Equal(t, "plain", cfg.Session.SASLMechanism)
	require.Equal(t, "bridge", cfg.Session.Username)
	require.Equal(t, "secret", cfg.Session.Password.Reveal())
	require.NotNil(t, cfg.Session.TLS)
	require.True(t, cfg.Session.TLS.Enable)

	require.Equal(t, "orders", cfg.Receiver.Address)
	require.Equal(t, uint32(50), cfg.Receiver.LinkCredit)
	require.Equal(t, uint32(2), cfg.Receiver.DurabilityMode)
	require.Equal(t, RoutingMulticast, cfg.Receiver.Routing)
	require.Equal(t, "orders-sub", cfg.Receiver.SubscriptionName)

	require.Equal(t, "orders-out", cfg.Sender.Address)
	require.Equal(t, 15*time.Second, cfg.Sender.Timeout)
	require.Equal(t, RoutingAnycast, cfg.Sender.Routing)
	require.NotNil(t, cfg.Sender.Durable)
	require.False(t, *cfg.Sender.Durable)

	require.Equal(t, "aws-secretsmanager://prod/amqp", cfg.CredentialsURIRef)
	require.NoError(t, cfg.Validate())
}
