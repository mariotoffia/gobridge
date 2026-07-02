package amqp10

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
