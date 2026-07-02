package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
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
// under session/sender must decode into the typed Config. Before the
// mapstructure/json tags were added these keys failed with
// "'session' has invalid keys: broker_urls, client_id" under ErrorUnused,
// leaving the transport unconfigurable via YAML.
func TestPluginOptionsDecode_Nested_Succeeds(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"client_id":       "c1",
			"broker_urls":     []any{"tcp://b:1883"},
			"keep_alive":      45,
			"connect_timeout": "10s",
			"clean_start":     false,
			"username":        "alice",
			"password":        "s3cr3t",
			"tls": map[string]any{
				"enable":               true,
				"ca_cert_file":         "/etc/ca.pem",
				"insecure_skip_verify": true,
			},
		},
		"sender": map[string]any{
			"default_topic": "out",
			"qos":           2,
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))

	require.Equal(t, "c1", cfg.Session.ClientID)
	require.Equal(t, []string{"tcp://b:1883"}, cfg.Session.BrokerURLs)
	require.Equal(t, uint16(45), cfg.Session.KeepAlive)
	require.Equal(t, 10*time.Second, cfg.Session.ConnectTimeout)
	require.False(t, cfg.Session.CleanStart)
	require.Equal(t, "alice", cfg.Session.Username)
	// Secret decodes from a scalar string via the production TextUnmarshaler
	// hook; the real value is reachable only through Reveal.
	require.Equal(t, "s3cr3t", cfg.Session.Password.Reveal())

	require.NotNil(t, cfg.Session.TLS)
	require.True(t, cfg.Session.TLS.Enable)
	require.Equal(t, "/etc/ca.pem", cfg.Session.TLS.CACertFile)
	require.True(t, cfg.Session.TLS.InsecureSkipVerify)

	require.Equal(t, "out", cfg.Sender.DefaultTopic)
	require.Equal(t, byte(2), cfg.Sender.QoS)
}

// TestPluginOptionsDecode_UnknownKey_Errors proves the strict (ErrorUnused)
// contract is intact after tagging: an undocumented nested key still fails
// the whole decode through the production decoder.
func TestPluginOptionsDecode_UnknownKey_Errors(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"client_id": "c1",
			"bogus_key": "nope",
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

// TestPluginOptionsDecode_BrokerURLSingularAlias proves the dominant documented
// single-broker form (`broker_url`, used in configuration-reference.md and
// scenarios 01-17) decodes through the real registry path: the decoder folds
// the singular alias into the canonical broker_urls list and clears it. Without
// the alias field + fold, ErrorUnused rejects `broker_url` and the most common
// MQTT config fails to load.
func TestPluginOptionsDecode_BrokerURLSingularAlias(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"client_id":  "c1",
			"broker_url": "tcp://b:1883",
		},
	}

	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	pc, err := reg.Decode("mqtt", parser.NewRawConfig(input))
	require.NoError(t, err)

	cfg, ok := pc.(*Config)
	require.True(t, ok)
	require.Equal(t, []string{"tcp://b:1883"}, cfg.Session.BrokerURLs)
	require.Empty(t, cfg.Session.BrokerURL)
}

// TestPluginOptionsDecode_BrokerURLsListWins proves the canonical list form
// takes precedence when both keys are present and that the singular alias is
// always cleared after normalization.
func TestPluginOptionsDecode_BrokerURLsListWins(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"client_id":   "c1",
			"broker_url":  "tcp://ignored:1883",
			"broker_urls": []any{"tcp://a:1883", "tcp://b:1883"},
		},
	}

	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	pc, err := reg.Decode("mqtt", parser.NewRawConfig(input))
	require.NoError(t, err)

	cfg, ok := pc.(*Config)
	require.True(t, ok)
	require.Equal(t, []string{"tcp://a:1883", "tcp://b:1883"}, cfg.Session.BrokerURLs)
	require.Empty(t, cfg.Session.BrokerURL)
}
