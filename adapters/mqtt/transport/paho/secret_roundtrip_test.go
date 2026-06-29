package paho

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestConfig_SecretRoundTrip proves the G1 secret contract for a plugin config:
// (1) a YAML/JSON scalar string decodes into the shared.Secret-typed
// Session.Password field (raw value reachable only via Reveal); (2) a DEFAULT
// json/yaml marshal REDACTS the value (defense in depth — a stray marshal must
// never leak); (3) the explicit persistence path (shared.RevealSecrets, used by
// the config save serializers) preserves the value so a save→reload cycle is
// lossless; and (4) the display surface (String) always redacts.
func TestConfig_SecretRoundTrip(t *testing.T) {
	const plaintext = "s3cr3t-passw0rd"
	raw := "session:\n  username: alice\n  password: " + plaintext + "\n"

	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfg))

	// Decoded: UnmarshalText populated the Secret; the real value is
	// reachable only through Reveal.
	require.False(t, cfg.Session.Password.IsZero())
	require.Equal(t, plaintext, cfg.Session.Password.Reveal())
	require.Equal(t, "alice", cfg.Session.Username)

	// Default marshal REDACTS (no explicit reveal): the value must not leak.
	yredacted, err := yaml.Marshal(&cfg)
	require.NoError(t, err)
	require.NotContains(t, string(yredacted), plaintext)
	require.Contains(t, string(yredacted), "[REDACTED]")

	jredacted, err := json.Marshal(&cfg)
	require.NoError(t, err)
	require.NotContains(t, string(jredacted), plaintext)
	require.Contains(t, string(jredacted), "[REDACTED]")

	// Persistence path: shared.RevealSecrets preserves the value, and the
	// re-marshalled artifact decodes back into the real Secret (lossless).
	yout, err := yaml.Marshal(shared.RevealSecrets(&cfg))
	require.NoError(t, err)
	require.Contains(t, string(yout), plaintext)
	var ycfg Config
	require.NoError(t, yaml.Unmarshal(yout, &ycfg))
	require.Equal(t, plaintext, ycfg.Session.Password.Reveal())

	jout, err := json.Marshal(shared.RevealSecrets(&cfg))
	require.NoError(t, err)
	require.True(t, strings.Contains(string(jout), plaintext))
	var jcfg Config
	require.NoError(t, json.Unmarshal(jout, &jcfg))
	require.Equal(t, plaintext, jcfg.Session.Password.Reveal())

	// Display/log surface redacts: the value never leaks through String/fmt.
	require.Equal(t, "[REDACTED]", cfg.Session.Password.String())
	require.NotContains(t, cfg.Session.Password.String(), plaintext)
}
