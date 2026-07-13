package paho

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// A-8 (MED): the map config path (tlsConfigFromMap) must parse the in-memory
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

// ═══════════════════════════════════════════════════════════════════════════
// A-15 (LOW): the crypto/rand-failure fallback for the client-id nonce must mix
// in PID and hostname. A bare timestamp fallback collides for two replicas
// started in the same tick on a host with a coarse clock, and identical
// client_ids trigger a mutual-takeover storm. Mixing PID (distinct per process)
// and hostname (distinct per host) keeps the token disambiguating even when the
// clock does not move.
//
// Mutation killed:
//   - drop the `%d` PID field from the seed → the differing-PID case collides
//     and its NotEqual assertion fails.
//   - drop the `%s` host field from the seed → the differing-host case collides
//     and its NotEqual assertion fails.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_ClientNonceFallback_MixesPIDAndHost(t *testing.T) {
	base := clientNonceFallback(1000, 42, "host-a")

	// Deterministic for identical inputs.
	assert.Equal(t, base, clientNonceFallback(1000, 42, "host-a"))

	// Same tick + same host, different process → must differ.
	assert.NotEqual(t, base, clientNonceFallback(1000, 43, "host-a"),
		"PID must disambiguate two replicas started in the same tick on one host")

	// Same tick + same process, different host → must differ.
	assert.NotEqual(t, base, clientNonceFallback(1000, 42, "host-b"),
		"hostname must disambiguate replicas on different hosts")

	// 4 bytes → 8 hex chars, matching the crypto/rand happy path width.
	assert.Len(t, base, 8)
}
