package paho

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// (MED): the map config path (tlsConfigFromMap) must parse the in-memory
// PEM keys (ca_cert_pem / cert_pem / key_pem) exactly like the typed decode
// path. Before the fix a library consumer wiring TLS through a map silently got
// system roots and no client certificate — an opaque auth failure that only
// surfaces at connect time.
//
// Mutation killed:
//   - drop any of the three `cfg.*PEM = shared.NewSecret(v)` lines → the
//     corresponding Reveal() assertion sees "" and fails.
//   - drop the `&& v != ""` empty guard → the empty-string case populates a
//     non-zero secret and the IsZero() assertion fails.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_TLSConfigFromMap_ParsesInlinePEM(t *testing.T) {
	cfg := tlsConfigFromMap(map[string]any{
		"enable":      true,
		"ca_cert_pem": "CA-PEM-BYTES",
		"cert_pem":    "CERT-PEM-BYTES",
		"key_pem":     "KEY-PEM-BYTES",
	})
	require.NotNil(t, cfg)
	assert.True(t, cfg.Enable)
	assert.Equal(t, "CA-PEM-BYTES", cfg.CACertPEM.Reveal(), "ca_cert_pem must be parsed")
	assert.Equal(t, "CERT-PEM-BYTES", cfg.CertPEM.Reveal(), "cert_pem must be parsed")
	assert.Equal(t, "KEY-PEM-BYTES", cfg.KeyPEM.Reveal(), "key_pem must be parsed")

	// An empty string is not PEM material and must leave the secret zero (so it
	// does not shadow a *_file fallback with an empty in-memory value).
	empty := tlsConfigFromMap(map[string]any{"ca_cert_pem": ""})
	assert.True(t, empty.CACertPEM.IsZero(), "empty ca_cert_pem must stay zero")
}

// TestBug_ClientNonceUses128Bits verifies the replica nonce has the required
// 128 bits of entropy and deterministic hex encoding for a supplied reader.
func TestBug_ClientNonceUses128Bits(t *testing.T) {
	nonce, err := randomClientNonce(bytes.NewReader(make([]byte, 16)))
	require.NoError(t, err)
	assert.Len(t, nonce, 32)
	assert.Equal(t, "00000000000000000000000000000000", nonce)
}
