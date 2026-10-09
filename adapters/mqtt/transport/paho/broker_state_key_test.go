package paho

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

func brokerStateTestConfig(clientID, brokerURL string) Config {
	cfg := DefaultConfig()
	cfg.Session.ClientID = clientID
	cfg.Session.BrokerURLs = []string{brokerURL}
	return cfg
}

func requireBrokerStateKey(t *testing.T, cfg Config, mode connectivity.SessionMode) string {
	t.Helper()
	key, err := cfg.BrokerStateKey(mode)
	require.NoError(t, err)
	require.NotEmpty(t, key)
	return key
}

func TestBrokerStateKey_IgnoresModeCleanStartExpiryAndProtocol(t *testing.T) {
	base := brokerStateTestConfig("orders", "tcp://broker.example:1883")
	want := requireBrokerStateKey(t, base, connectivity.SessionPersistent)

	assert.Equal(t, want, requireBrokerStateKey(t, base, connectivity.SessionExclusive), "mode")
	cleanStart := base
	cleanStart.Session.CleanStart = true
	assert.Equal(t, want, requireBrokerStateKey(t, cleanStart, connectivity.SessionPersistent), "clean start")
	expiry := base
	expiry.Session.SessionExpiryInterval = 600
	assert.Equal(t, want, requireBrokerStateKey(t, expiry, connectivity.SessionPersistent), "session expiry")
	v311 := base
	v311.Session.ProtocolVersion = ProtocolVersion311
	assert.Equal(t, want, requireBrokerStateKey(t, v311, connectivity.SessionPersistent), "protocol version")
}

func TestBrokerStateKey_ChangesWithTheClientIDOrTheBroker(t *testing.T) {
	want := requireBrokerStateKey(t, brokerStateTestConfig("orders", "tcp://broker.example:1883"), connectivity.SessionPersistent)

	assert.NotEqual(t, want, requireBrokerStateKey(t,
		brokerStateTestConfig("orders-renamed", "tcp://broker.example:1883"), connectivity.SessionPersistent))
	assert.NotEqual(t, want, requireBrokerStateKey(t,
		brokerStateTestConfig("orders", "tcp://other.example:1883"), connectivity.SessionPersistent))
}

func TestBrokerStateKey_SpellingsOfOneEndpointShareAKey(t *testing.T) {
	assert.Equal(t,
		requireBrokerStateKey(t, brokerStateTestConfig("orders", "tcp://broker.example:1883"), connectivity.SessionPersistent),
		requireBrokerStateKey(t, brokerStateTestConfig("orders", "mqtt://Broker.Example"), connectivity.SessionPersistent))
}

func TestBrokerStateKey_CarriesNeitherTheClientIDNorTheBroker(t *testing.T) {
	key := requireBrokerStateKey(t, brokerStateTestConfig("orders", "tcp://broker.example:1883"), connectivity.SessionPersistent)

	assert.True(t, strings.HasPrefix(key, "mqtt:"), "the key names its transport")
	assert.NotContains(t, key, "orders")
	assert.NotContains(t, key, "broker.example")
}

func TestBrokerStateKey_EphemeralSessionHasNone(t *testing.T) {
	cfg := brokerStateTestConfig("orders", "tcp://broker.example:1883")
	for _, mode := range []connectivity.SessionMode{"", connectivity.SessionEphemeral} {
		key, err := cfg.BrokerStateKey(mode)
		require.NoError(t, err)
		assert.Empty(t, key, "mode %q", mode)
	}
}

func TestManagedSubscriptionIdentity_IsTheDigestOfTheBrokerStateKey(t *testing.T) {
	cfg := brokerStateTestConfig("orders", "tcp://broker.example:1883")
	key := requireBrokerStateKey(t, cfg, connectivity.SessionPersistent)

	identity, err := cfg.ManagedSubscriptionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.Equal(t, identityDigest(key), identity)

	legacy, err := cfg.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.NotEqual(t, legacy, identity, "the history moves off the old fingerprint")
}

func TestManagedSubscriptionIdentity_SurvivesAnExpiryChangeTheOldFingerprintDidNot(t *testing.T) {
	base := brokerStateTestConfig("orders", "tcp://broker.example:1883")
	changed := base
	changed.Session.SessionExpiryInterval = 600

	baseIdentity, err := base.ManagedSubscriptionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	changedIdentity, err := changed.ManagedSubscriptionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.Equal(t, baseIdentity, changedIdentity)

	baseLegacy, err := base.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	changedLegacy, err := changed.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	assert.NotEqual(t, baseLegacy, changedLegacy)
}

// brokerStateForeignConfig is a plugin config of another transport.
type brokerStateForeignConfig struct{}

func (brokerStateForeignConfig) Kind() string    { return "foreign" }
func (brokerStateForeignConfig) Validate() error { return nil }

func TestFactoryBrokerStateKeys_KeysADurableSessionByItsBrokerStateKey(t *testing.T) {
	cfg := brokerStateTestConfig("orders", "tcp://broker.example:1883")
	want := requireBrokerStateKey(t, cfg, connectivity.SessionExclusive)

	keys, err := NewFactory(nil).BrokerStateKeys(
		ports.SessionSpec{ID: "orders", SessionMode: connectivity.SessionExclusive, Config: &cfg}, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{want}, keys)
}

func TestFactoryBrokerStateKeys_EphemeralSessionHasNoKey(t *testing.T) {
	cfg := brokerStateTestConfig("orders", "tcp://broker.example:1883")

	keys, err := NewFactory(nil).BrokerStateKeys(ports.SessionSpec{ID: "orders", Config: cfg}, nil)

	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestFactoryBrokerStateKeys_RejectsAForeignConfig(t *testing.T) {
	_, err := NewFactory(nil).BrokerStateKeys(ports.SessionSpec{
		ID: "orders", SessionMode: connectivity.SessionPersistent, Config: brokerStateForeignConfig{},
	}, nil)

	require.Error(t, err)
}
