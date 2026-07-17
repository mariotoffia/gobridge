package paho

import (
	"errors"
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
		BrokerURLs:            []string{"ssl://broker-a.example:8883"},
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

func TestConfig_ValidateSessionModeRejectsIndependentDurableBrokerURLs(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.BrokerURLs = []string{"ssl://broker-a.example:8883", "ssl://broker-b.example:8883"}

	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		require.Error(t, cfg.ValidateSessionMode(mode), "durable mode %s must not span independent broker-session domains", mode)
	}
	require.NoError(t, cfg.ValidateSessionMode(connectivity.SessionEphemeral),
		"ephemeral failover may use independent broker URLs")

	duplicate := cloneIdentityConfig(cfg)
	duplicate.Session.BrokerURLs = []string{
		"ssl://BROKER-A.example:8883?a=1&b=2",
		"ssl://user:pass@broker-a.example:8883?b=2&a=1",
	}
	require.NoError(t, duplicate.ValidateSessionMode(connectivity.SessionPersistent),
		"canonical duplicates prove one broker-session domain")
}

func TestConfig_DurableSessionIdentity_ChangesForEveryBrokerStateField(t *testing.T) {
	base := durableIdentityConfig()
	baseID := identityFingerprint(t, base, connectivity.SessionPersistent)

	tests := map[string]func(*Config){
		"broker set":          func(c *Config) { c.Session.BrokerURLs[0] = "ssl://broker-c.example:8883" },
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

func TestConfig_DurableSessionIdentityDomains_RejectClientCollisionAcrossStateSettings(t *testing.T) {
	first := durableIdentityConfig()
	second := cloneIdentityConfig(first)
	second.Session.CleanStart = true
	second.Session.SessionExpiryInterval++

	firstState := identityFingerprint(t, first, connectivity.SessionPersistent)
	secondState := identityFingerprint(t, second, connectivity.SessionPersistent)
	require.NotEqual(t, firstState, secondState)

	firstDomains, err := first.DurableSessionIdentityDomains(connectivity.SessionPersistent)
	require.NoError(t, err)
	secondDomains, err := second.DurableSessionIdentityDomains(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.Equal(t, firstDomains, secondDomains, "same brokers and effective client ID collide regardless of state settings")
}

func TestConfig_DurableSessionIdentity_EphemeralBrokerOrderAndDuplicatesAreIdentity(t *testing.T) {
	base := durableIdentityConfig()
	base.Session.BrokerURLs = []string{"ssl://broker-a.example:8883", "ssl://broker-b.example:8883"}
	want := identityFingerprint(t, base, connectivity.SessionEphemeral)

	reordered := cloneIdentityConfig(base)
	reordered.Session.BrokerURLs[0], reordered.Session.BrokerURLs[1] =
		reordered.Session.BrokerURLs[1], reordered.Session.BrokerURLs[0]
	assert.NotEqual(t, want, identityFingerprint(t, reordered, connectivity.SessionEphemeral),
		"Paho attempts ephemeral failover brokers in configured order")

	duplicated := cloneIdentityConfig(base)
	duplicated.Session.BrokerURLs = append(duplicated.Session.BrokerURLs, duplicated.Session.BrokerURLs[0])
	assert.NotEqual(t, want, identityFingerprint(t, duplicated, connectivity.SessionEphemeral),
		"Paho does not deduplicate configured ephemeral brokers")
	_, err := base.DurableSessionIdentity(connectivity.SessionPersistent)
	require.Error(t, err, "durable identity must reject independent broker domains")
}

func TestConfig_IdentityCapabilities_ValueAndPointerParity(t *testing.T) {
	value := *durableIdentityConfig()
	pointer := &value

	valueIdentity, valueOK := any(value).(ports.DurableSessionIdentityConfig)
	pointerIdentity, pointerOK := any(pointer).(ports.DurableSessionIdentityConfig)
	require.True(t, valueOK, "factory-supported value Config must expose durable identity")
	require.True(t, pointerOK)

	valueFingerprint, err := valueIdentity.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	pointerFingerprint, err := pointerIdentity.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.Equal(t, pointerFingerprint, valueFingerprint)

	valueReplica, valueOK := any(value).(ports.ReplicaIdentityConfig)
	pointerReplica, pointerOK := any(pointer).(ports.ReplicaIdentityConfig)
	require.True(t, valueOK, "factory-supported value Config must expose replica identity")
	require.True(t, pointerOK)
	assert.Equal(t, pointerReplica.ReplicaIdentityStrategy(), valueReplica.ReplicaIdentityStrategy())
}

func TestConfig_DurableSessionIdentity_CanonicalAndSecretSafe(t *testing.T) {
	base := durableIdentityConfig()
	want := identityFingerprint(t, base, connectivity.SessionPersistent)

	canonical := cloneIdentityConfig(base)
	canonical.Session.BrokerURLs = []string{"ssl://user:rotated@BROKER-A.example:8883"}
	assert.Equal(t, want, identityFingerprint(t, canonical, connectivity.SessionPersistent),
		"per-URL spelling and URL credentials are not identity")

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
	cfg.Session.BrokerURLs = cfg.Session.BrokerURLs[:1]
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

type failingNonceReader struct{}

func (failingNonceReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestConfig_ClientIDNonceEntropyFailurePropagatesWithoutFallback(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixNonce
	cfg.clientIDSuffixIdentity = &clientIDSuffixProcessIdentity{random: failingNonceReader{}}

	fingerprint, err := cfg.DurableSessionIdentity(connectivity.SessionEphemeral)
	require.Error(t, err)
	assert.Empty(t, fingerprint)
	assert.Contains(t, err.Error(), "entropy unavailable")
	assert.NotContains(t, err.Error(), cfg.Session.ClientID)

	copied := *cfg
	_, err = NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID: "session", SessionMode: connectivity.SessionEphemeral, Config: copied,
	})
	require.Error(t, err, "session construction must not silently fall back")
	assert.Contains(t, err.Error(), "invalid client_id_suffix")
}

func TestConfig_ClientIDNonceStableAcrossValueCopies(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.ClientIDSuffix = ClientIDSuffixNonce
	cfg.clientIDSuffixIdentity = &clientIDSuffixProcessIdentity{
		random: strings.NewReader("0123456789abcdef"),
	}
	copied := *cfg

	first, err := cfg.resolveClientIDSuffix(cfg.Session.ClientID, ClientIDSuffixNonce)
	require.NoError(t, err)
	second, err := copied.resolveClientIDSuffix(copied.Session.ClientID, ClientIDSuffixNonce)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Len(t, strings.TrimPrefix(first, cfg.Session.ClientID+"-"), 32)
}

func TestConfig_DurableSessionIdentityDomains_ArePerCanonicalEndpoint(t *testing.T) {
	type endpointDomains interface {
		DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error)
	}

	first := durableIdentityConfig()
	first.Session.BrokerURLs = []string{"ssl://broker-a.example:8883", "ssl://broker-b.example:8883"}
	overlap := cloneIdentityConfig(first)
	overlap.Session.BrokerURLs = []string{"ssl://BROKER-A.example:8883", "ssl://broker-c.example:8883"}
	disjoint := cloneIdentityConfig(first)
	disjoint.Session.BrokerURLs = []string{"ssl://broker-c.example:8883", "ssl://broker-d.example:8883"}

	capability, ok := any(first).(endpointDomains)
	require.True(t, ok, "Paho must expose one opaque ownership domain per broker endpoint")
	_, err := capability.DurableSessionIdentityDomains(connectivity.SessionPersistent)
	require.Error(t, err, "durable ownership domains must reject independent brokers")
	firstDomains, err := capability.DurableSessionIdentityDomains(connectivity.SessionEphemeral)
	require.NoError(t, err)
	overlapDomains, err := any(overlap).(endpointDomains).DurableSessionIdentityDomains(connectivity.SessionEphemeral)
	require.NoError(t, err)
	disjointDomains, err := any(disjoint).(endpointDomains).DurableSessionIdentityDomains(connectivity.SessionEphemeral)
	require.NoError(t, err)

	assert.Len(t, firstDomains, 2)
	assert.True(t, stringSetsOverlap(firstDomains, overlapDomains))
	assert.False(t, stringSetsOverlap(firstDomains, disjointDomains))
	for _, domain := range append(append(firstDomains, overlapDomains...), disjointDomains...) {
		assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), domain)
	}
}

func stringSetsOverlap(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := seen[value]; ok {
			return true
		}
	}
	return false
}

func TestConfig_FreezePluginConfig_DeepOwnsConfigAndSharesRuntimeIdentity(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.TLS = &TLSConfig{CACertFile: "ca.pem"}
	cfg.Session.Will = &WillOptions{Topic: "status", Payload: "offline"}
	cfg.CredentialsURIRef = "vault://mqtt"
	state := &clientIDSuffixProcessIdentity{}
	cfg.clientIDSuffixIdentity = state

	frozen, ok := cfg.FreezePluginConfig().(*Config)
	require.True(t, ok)
	require.NotSame(t, cfg, frozen)
	assert.NotSame(t, cfg.Session.TLS, frozen.Session.TLS)
	assert.NotSame(t, cfg.Session.Will, frozen.Session.Will)
	assert.Same(t, state, frozen.clientIDSuffixIdentity,
		"process-stable suffix dependency must retain identity")
	assert.Equal(t, cfg.Session.Clock, frozen.Session.Clock,
		"opaque clock dependency must be preserved intentionally")
	require.NoError(t, frozen.ApplyCredentials(nil))
	assert.Equal(t, "vault://mqtt", cfg.CredentialsURIRef,
		"credential application to the frozen build config must not corrupt rollback state")
	assert.Empty(t, frozen.CredentialsURIRef)

	cfg.Session.BrokerURLs[0] = "ssl://mutated.example:8883"
	cfg.Session.TLS.CACertFile = "mutated.pem"
	cfg.Session.Will.Topic = "mutated"
	assert.NotEqual(t, cfg.Session.BrokerURLs, frozen.Session.BrokerURLs)
	assert.Equal(t, "ca.pem", frozen.Session.TLS.CACertFile)
	assert.Equal(t, "status", frozen.Session.Will.Topic)
}
