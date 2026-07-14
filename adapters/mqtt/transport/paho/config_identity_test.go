package paho

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func durableIdentityConfig() *Config {
	return &Config{Session: SessionOptions{
		BrokerURLs:            []string{"ssl://user:old-secret@broker-b.example:8883?b=2&a=1", "ssl://broker-a.example:8883"},
		ClientID:              "bridge-client",
		SessionExpiryInterval: 3600,
	}}
}

func identityFingerprint(t *testing.T, cfg *Config, mode connectivity.SessionMode) string {
	t.Helper()
	got, err := cfg.DurableSessionIdentity(mode)
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), got)
	return got
}

func cloneIdentityConfig(c *Config) *Config {
	clone := *c
	clone.Session = c.Session
	clone.Session.BrokerURLs = append([]string(nil), c.Session.BrokerURLs...)
	if c.Session.TLS != nil {
		tlsClone := *c.Session.TLS
		clone.Session.TLS = &tlsClone
	}
	return &clone
}

func TestConfig_DurableSessionIdentity_ChangesForEveryBrokerStateField(t *testing.T) {
	base := durableIdentityConfig()
	baseID := identityFingerprint(t, base, connectivity.SessionPersistent)

	tests := map[string]func(*Config){
		"broker set":          func(c *Config) { c.Session.BrokerURLs[1] = "ssl://broker-c.example:8883" },
		"effective client ID": func(c *Config) { c.Session.ClientID = "other-client" },
		"client ID suffix":    func(c *Config) { c.Session.ClientIDSuffix = ClientIDSuffixHostname },
		"clean start":         func(c *Config) { c.Session.CleanStart = true },
		"session expiry":      func(c *Config) { c.Session.SessionExpiryInterval++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := cloneIdentityConfig(base)
			mutate(changed)
			assert.NotEqual(t, baseID, identityFingerprint(t, changed, connectivity.SessionPersistent))
		})
	}
	assert.NotEqual(t, baseID, identityFingerprint(t, base, connectivity.SessionEphemeral), "session mode is identity")
}

func TestConfig_DurableSessionIdentity_CanonicalAndSecretSafe(t *testing.T) {
	base := durableIdentityConfig()
	want := identityFingerprint(t, base, connectivity.SessionPersistent)

	canonical := cloneIdentityConfig(base)
	canonical.Session.BrokerURLs = []string{
		"ssl://broker-a.example:8883",
		"ssl://other-user:new-secret@broker-b.example:8883?a=1&b=2",
		"ssl://broker-a.example:8883",
	}
	assert.Equal(t, want, identityFingerprint(t, canonical, connectivity.SessionPersistent),
		"broker order, duplicates, and URL credentials are not identity")

	nonIdentity := cloneIdentityConfig(base)
	nonIdentity.CredentialsURIRef = "vault://rotated/mqtt"
	nonIdentity.Session.Username = "new-user"
	nonIdentity.Session.Password = shared.NewSecret("new-password")
	nonIdentity.Session.KeepAlive = 9
	nonIdentity.Session.ConnectTimeout = time.Second
	nonIdentity.Session.ReconnectTimeout = 2 * time.Second
	nonIdentity.Session.ReconcileTimeout = 3 * time.Second
	nonIdentity.Session.ReconnectDelay = 4 * time.Second
	nonIdentity.Session.ReconnectMaxDelay = 5 * time.Second
	nonIdentity.Session.UnmatchedGrace = 6 * time.Second
	nonIdentity.Session.ReceiveMaximum = 7
	nonIdentity.Session.MaxPayloadBytes = 8
	nonIdentity.Session.TLS = &TLSConfig{
		Enable:     true,
		CACertFile: "/rotated/ca.pem",
		CertFile:   "/rotated/cert.pem",
		KeyFile:    "/rotated/key.pem",
		CACertPEM:  shared.NewSecret("ca-secret"),
		CertPEM:    shared.NewSecret("cert-secret"),
		KeyPEM:     shared.NewSecret("key-secret"),
	}
	assert.Equal(t, want, identityFingerprint(t, nonIdentity, connectivity.SessionPersistent))

	got, err := canonical.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	for _, secret := range []string{"old-secret", "new-secret", "new-password", "key-secret"} {
		assert.NotContains(t, got, secret)
	}
}

func TestConfig_DurableSessionIdentity_URLParseErrorRedactsUserInfo(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.BrokerURLs = []string{"%gh://user:url-secret@broker.example"}
	got, err := cfg.DurableSessionIdentity(connectivity.SessionPersistent)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.NotContains(t, err.Error(), "url-secret")
	assert.NotContains(t, err.Error(), cfg.Session.BrokerURLs[0])
}

func TestConfig_DurableSessionIdentity_UsesEffectiveWireBehavior(t *testing.T) {
	base := durableIdentityConfig()

	ephemeralA := cloneIdentityConfig(base)
	ephemeralB := cloneIdentityConfig(base)
	ephemeralB.Session.CleanStart = true
	ephemeralB.Session.SessionExpiryInterval = 999
	assert.Equal(t,
		identityFingerprint(t, ephemeralA, connectivity.SessionEphemeral),
		identityFingerprint(t, ephemeralB, connectivity.SessionEphemeral))

	exclusiveA := cloneIdentityConfig(base)
	exclusiveB := cloneIdentityConfig(base)
	exclusiveB.Session.CleanStart = true
	assert.Equal(t,
		identityFingerprint(t, exclusiveA, connectivity.SessionExclusive),
		identityFingerprint(t, exclusiveB, connectivity.SessionExclusive))

	persistentDefault := cloneIdentityConfig(base)
	persistentDefault.Session.SessionExpiryInterval = 0
	persistentExplicit := cloneIdentityConfig(base)
	persistentExplicit.Session.SessionExpiryInterval = DefaultPersistentSessionExpiry
	assert.Equal(t,
		identityFingerprint(t, persistentDefault, connectivity.SessionPersistent),
		identityFingerprint(t, persistentExplicit, connectivity.SessionPersistent))
}

func TestConfig_DurableSessionIdentity_NonceIsStableOnlyForEphemeral(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixNonce

	first := identityFingerprint(t, cfg, connectivity.SessionEphemeral)
	second := identityFingerprint(t, cfg, connectivity.SessionEphemeral)
	assert.Equal(t, first, second, "nonce identity must remain stable for all reload checks in one process")

	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		_, err := cfg.DurableSessionIdentity(mode)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), cfg.Session.ClientID)
	}
}

func TestConfig_ReplicaIdentityStrategy(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixHostname
	assert.Equal(t, ClientIDSuffixHostname, cfg.ReplicaIdentityStrategy())
	var _ ports.DurableSessionIdentityConfig = cfg
	var _ ports.ReplicaIdentityConfig = cfg
}

func TestFactoryNewSession_ClientIDSuffixNonce_RejectsDurableModes(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixNonce
	factory := NewFactory(nil)
	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		_, err := factory.NewSession(t.Context(), ports.SessionSpec{ID: "session", SessionMode: mode, Config: cfg})
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "nonce") || strings.Contains(err.Error(), "client_id_suffix"))
	}
}

func TestFactoryNewSession_ClientIDSuffixNonce_AllowsDefaultEphemeralMode(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixNonce
	factory := NewFactory(nil)

	session, err := factory.NewSession(t.Context(), ports.SessionSpec{ID: "session", Config: cfg})
	require.NoError(t, err)
	require.NotNil(t, session)
}
