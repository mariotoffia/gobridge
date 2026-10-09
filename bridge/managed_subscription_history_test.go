package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// legacyOnlyIdentityConfig exposes the old fingerprint and not the managed
// subscription identity.
type legacyOnlyIdentityConfig struct{}

func (legacyOnlyIdentityConfig) Kind() string    { return "mqtt" }
func (legacyOnlyIdentityConfig) Validate() error { return nil }
func (legacyOnlyIdentityConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return "safe-durable-fingerprint", nil
}
func (legacyOnlyIdentityConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	return []string{"safe-domain"}, nil
}

func TestSessionSpecWithManagedSubscriptions_RefusesAConfigWithoutTheHistoryIdentity(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions:  []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent", Config: legacyOnlyIdentityConfig{}}},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "mqtt-sess", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}

	_, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, managedSpecStore{})

	require.ErrorContains(t, err, "managed subscription storage identity")
}

func TestSeedManagedSubscriptionBaselines_CarriesTheLegacyHistoryOverBeforeSeeding(t *testing.T) {
	store := newHistoryStore(map[string][]string{"safe-durable-fingerprint": {"orders/legacy/#"}})
	b := NewBuilder(seedTestConfig("persistent")).RegisterStoreFactory("recording", &historyStoreFactory{store: store})

	require.NoError(t, b.SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{"durable": {"orders/new/#"}}))

	got, established := store.history("safe-managed-identity")
	require.True(t, established)
	assert.Equal(t, []string{"orders/legacy/#", "orders/new/#"}, got,
		"seeding must not hide the history the session kept under the old fingerprint")
}

func TestSeedManagedSubscriptionBaselines_KeepsAHistoryTheNewKeyAlreadyHas(t *testing.T) {
	store := newHistoryStore(map[string][]string{
		"safe-durable-fingerprint": {"orders/legacy/#"},
		"safe-managed-identity":    {"orders/current/#"},
	})
	b := NewBuilder(seedTestConfig("persistent")).RegisterStoreFactory("recording", &historyStoreFactory{store: store})

	require.NoError(t, b.SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{"durable": nil}))

	got, _ := store.history("safe-managed-identity")
	assert.Equal(t, []string{"orders/current/#"}, got)
}

func TestSeedManagedSubscriptionBaselines_RefusesASessionWithoutTheHistoryIdentity(t *testing.T) {
	cfg := seedTestConfig("persistent")
	cfg.Sessions[0].Config = legacyOnlyIdentityConfig{}
	b := NewBuilder(cfg).RegisterStoreFactory("recording", &historyStoreFactory{store: newHistoryStore(map[string][]string{})})

	err := b.SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{"durable": nil})

	require.ErrorContains(t, err, "managed subscription storage identity")
}
