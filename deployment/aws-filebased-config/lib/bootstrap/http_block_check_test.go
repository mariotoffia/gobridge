package bootstrap

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestCheckIgnoredHTTPBlock_FailsClosedOnTLSPair pins Chunk 16 Finding 2 (Fix 2):
// the file-based profile cannot serve in-process TLS (TLS terminates at the
// load balancer), so a tls_cert_file/tls_key_file entry in the bridge config
// `http:` block is an "encrypt this" instruction the profile cannot honor.
// Continuing would silently serve the admin API in plaintext, so the check must
// FAIL CLOSED — return a non-nil error that names tls_cert_file/tls_key_file and
// never leaks the API-key secret — rather than warn and continue. A half-set
// pair (only cert or only key) is rejected too, since the profile serves no TLS
// at all.
func TestCheckIgnoredHTTPBlock_FailsClosedOnTLSPair(t *testing.T) {
	const apiKey = "super-secret-admin-key"
	for _, tc := range []struct {
		name string
		http *ports.HTTPConfig
	}{
		{
			name: "full pair",
			http: &ports.HTTPConfig{
				AdminAddr:   ":8080",
				MonitorAddr: ":9090",
				AdminAPIKey: shared.NewSecret(apiKey),
				TLSCertFile: "/etc/tls/server.crt",
				TLSKeyFile:  "/etc/tls/server.key",
			},
		},
		{
			name: "only cert",
			http: &ports.HTTPConfig{
				AdminAPIKey: shared.NewSecret(apiKey),
				TLSCertFile: "/etc/tls/server.crt",
			},
		},
		{
			name: "only key",
			http: &ports.HTTPConfig{
				AdminAPIKey: shared.NewSecret(apiKey),
				TLSKeyFile:  "/etc/tls/server.key",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))

			err := checkIgnoredHTTPBlock(logger, &ports.BridgeConfig{HTTP: tc.http})

			require.Error(t, err, "a TLS entry must fail closed")
			assert.Contains(t, err.Error(), "tls_cert_file/tls_key_file")
			assert.Contains(t, err.Error(), "plaintext")
			// The failure aborts before warning; nothing should be logged.
			assert.Empty(t, buf.String(), "fail-closed path must not emit a WARN")
			// The admin API key is a secret and must never appear in the error.
			assert.NotContains(t, err.Error(), apiKey)
		})
	}
}

// TestCheckIgnoredHTTPBlock_WarnsAndContinuesWithoutTLSPair pins the middle
// case: an `http:` block that carries only addresses/keys (no TLS entry) is
// legitimately overridden by the SSM-driven bootstrap, so the check returns nil
// (startup continues) but emits a loud WARN. The warning names the ignored
// block, reports the addresses and that no TLS pair is set, and NEVER logs the
// API-key secret.
func TestCheckIgnoredHTTPBlock_WarnsAndContinuesWithoutTLSPair(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	const apiKey = "super-secret-admin-key"
	logical := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{
			AdminAddr:   ":8080",
			MonitorAddr: ":9090",
			AdminAPIKey: shared.NewSecret(apiKey),
		},
	}

	err := checkIgnoredHTTPBlock(logger, logical)
	require.NoError(t, err, "a non-TLS http: block must warn-and-continue, not fail")

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "expected exactly one JSON warn line")
	assert.Equal(t, "WARN", line["level"])
	msg, _ := line["msg"].(string)
	assert.Contains(t, msg, "does NOT honor")
	assert.Contains(t, msg, "http:")
	assert.Equal(t, ":8080", line["admin_addr"])
	assert.Equal(t, ":9090", line["monitor_addr"])
	assert.Equal(t, false, line["tls_cert_file_set"])
	assert.Equal(t, false, line["tls_key_file_set"])
	// The admin API key is a secret and must never appear in the warning.
	assert.NotContains(t, buf.String(), apiKey)
}

// TestCheckIgnoredHTTPBlock_SilentWhenNoHTTPBlock pins the last case: the normal
// file-based deployment (no `http:` block) starts quietly and returns nil. A nil
// config is covered defensively — the check is called from Start with the loaded
// config.
func TestCheckIgnoredHTTPBlock_SilentWhenNoHTTPBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logical *ports.BridgeConfig
	}{
		{"no http block", &ports.BridgeConfig{}},
		{"nil config", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))

			err := checkIgnoredHTTPBlock(logger, tc.logical)

			require.NoError(t, err, "absent http: block must not fail closed")
			assert.Empty(t, buf.String(), "warn must not fire when the http: block is absent")
		})
	}
}
