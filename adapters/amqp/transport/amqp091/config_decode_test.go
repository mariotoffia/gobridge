package amqp091

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
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
// mapstructure/json tags were added session.broker_url (and siblings)
// failed under ErrorUnused, leaving the transport unconfigurable via YAML.
func TestPluginOptionsDecode_Nested_Succeeds(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"broker_url":           "amqp://user@host:5672/",
			"heartbeat":            "15s",
			"connect_timeout":      "10s",
			"reconnect_max_delay":  "45s",
			"reconnect_multiplier": 2.5,
			"username":             "bob",
			"password":             "pw",
			"vhost":                "/prod",
			"tls": map[string]any{
				"enable":    true,
				"cert_file": "/c.pem",
				"key_file":  "/k.pem",
			},
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))

	require.Equal(t, "amqp://user@host:5672/", cfg.Session.BrokerURL)
	require.Equal(t, 15*time.Second, cfg.Session.Heartbeat)
	require.Equal(t, 10*time.Second, cfg.Session.ConnectTimeout)
	require.Equal(t, 45*time.Second, cfg.Session.ReconnectMaxDelay)
	require.Equal(t, 2.5, cfg.Session.ReconnectMultiplier)
	require.Equal(t, "bob", cfg.Session.Username)
	// Secret decodes from a scalar string via the production TextUnmarshaler
	// hook; the real value is reachable only through Reveal.
	require.Equal(t, "pw", cfg.Session.Password.Reveal())
	require.Equal(t, "/prod", cfg.Session.Vhost)

	require.NotNil(t, cfg.Session.TLS)
	require.True(t, cfg.Session.TLS.Enable)
	require.Equal(t, "/c.pem", cfg.Session.TLS.CertFile)
	require.Equal(t, "/k.pem", cfg.Session.TLS.KeyFile)
}

// TestPluginOptionsDecode_UnknownKey_Errors proves the strict (ErrorUnused)
// contract is intact after tagging: an undocumented nested key still fails
// the whole decode through the production decoder.
func TestPluginOptionsDecode_UnknownKey_Errors(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"broker_url": "amqp://host:5672/",
			"bogus_key":  "nope",
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

// TestPluginOptionsDecode_AllowUnroutableDrop proves the new
// sender.allow_unroutable_drop opt-in decodes through the production
// (strict, TagName=json) decoder — i.e. it is a RECOGNIZED key, not an
// ErrorUnused rejection — and lands in the typed SenderParams. Without the
// json:"allow_unroutable_drop" tag on the field this decode would fail.
func TestPluginOptionsDecode_AllowUnroutableDrop(t *testing.T) {
	input := map[string]any{
		"sender": map[string]any{
			"exchange":              "events",
			"mandatory":             false,
			"allow_unroutable_drop": true,
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))
	require.False(t, cfg.Sender.Mandatory)
	require.True(t, cfg.Sender.AllowUnroutableDrop)
}
