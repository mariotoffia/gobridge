package bridge

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// addedKeyBuildConfig has one persistent MQTT session, "stable-session", that
// keeps managed subscription history in the "history" store.
func addedKeyBuildConfig() *ports.BridgeConfig {
	cfg := configWithDurableSessionIdentity(1, "unused")
	cfg.Stores.ManagedSubscriptions = &ports.StoreConfig{Type: "history"}
	cfg.Sessions[0].Transport = "mqtt"
	cfg.Sessions[0].Config = freezableManagedIdentityConfig{}
	cfg.Senders[0].Transport = "mqtt"
	return cfg
}

// buildMarkingAddedKeys builds addedKeyBuildConfig over store with the sessions
// in added marked, and returns the spec its session was created from.
func buildMarkingAddedKeys(t *testing.T, store *historyStore, added []string) ports.SessionSpec {
	t.Helper()
	var (
		mu    sync.Mutex
		built ports.SessionSpec
	)
	factory := &countingTransportFactory{}
	factory.SessionFn = func(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		built = spec
		return &fakeSession{}, nil
	}
	rt, err := NewBuilder(addedKeyBuildConfig()).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterTransportFactory("mqtt", factory).
		RegisterStoreFactory("history", &historyStoreFactory{store: store}).
		MarkAddedBrokerStateKeys(added).
		Build(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	mu.Lock()
	defer mu.Unlock()
	return built
}

func TestBuild_RecordsAnEmptyHistoryForABrokerStateKeyAReloadAdded(t *testing.T) {
	store := newHistoryStore(map[string][]string{})

	spec := buildMarkingAddedKeys(t, store, []string{"stable-session"})

	assert.True(t, spec.BrokerStateKeyAdded)
	history, established := store.history("safe-managed-identity")
	require.True(t, established, "the session must find a baseline, not an unknown history")
	assert.Empty(t, history)
}

func TestBuild_KeepsAnExistingHistoryOfABrokerStateKeyAReloadAdded(t *testing.T) {
	// The key was used before (or another cluster member already filled its
	// history). Emptying it would forget filters the broker may still hold.
	store := newHistoryStore(map[string][]string{"safe-managed-identity": {"orders/#"}})

	spec := buildMarkingAddedKeys(t, store, []string{"stable-session"})

	assert.True(t, spec.BrokerStateKeyAdded)
	history, established := store.history("safe-managed-identity")
	require.True(t, established)
	assert.Equal(t, []string{"orders/#"}, history)
}

// TestBuild_CarriesTheLegacyHistoryOverForABrokerStateKeyAReloadAdded pins
// that an added key whose client ID still has history under the legacy
// fingerprint gets that history, not an empty baseline: an empty one would
// hide it, and the session would end a broker session whose history exists.
//
// Mutation check: drop the carry-over from the added-key branch of the build
// and the new key holds an empty history.
func TestBuild_CarriesTheLegacyHistoryOverForABrokerStateKeyAReloadAdded(t *testing.T) {
	store := newHistoryStore(map[string][]string{"safe-durable-fingerprint": {"orders/#"}})

	spec := buildMarkingAddedKeys(t, store, []string{"stable-session"})

	assert.True(t, spec.BrokerStateKeyAdded)
	history, established := store.history("safe-managed-identity")
	require.True(t, established)
	assert.Equal(t, []string{"orders/#"}, history)
}

func TestBuild_KeepsTheHistoryOfABrokerStateKeyNoReloadAdded(t *testing.T) {
	store := newHistoryStore(map[string][]string{"safe-managed-identity": {"orders/#"}})

	spec := buildMarkingAddedKeys(t, store, nil)

	assert.False(t, spec.BrokerStateKeyAdded)
	history, _ := store.history("safe-managed-identity")
	assert.Equal(t, []string{"orders/#"}, history)
}
