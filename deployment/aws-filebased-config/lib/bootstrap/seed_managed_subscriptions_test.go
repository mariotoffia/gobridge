package bootstrap

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// A persistent or exclusive MQTT session does not start until its
// managed-subscription baseline exists, and on this profile the only process
// that can write a SQLite store on the config mount is the task itself. The
// bootstrap document carries the attestation and the App seeds it at every
// boot, before it builds the bridge, through the same builder and store
// factories the runtime uses.

func durableIngressConfig(historyPath string) (*ports.BridgeConfig, *paho.Config) {
	mqtt := &paho.Config{Session: paho.SessionOptions{
		BrokerURLs: []string{"tcp://broker.example:1883"}, ClientID: "gobridge-single",
		ConnectTimeout: 5 * time.Second, KeepAlive: 10,
	}}
	session := ports.SessionDef{ID: "mqtt-in", Transport: "mqtt", SessionMode: string(connectivity.SessionPersistent)}
	session.SetDecoded(mqtt, nil)
	receiver := ports.ReceiverDef{
		ID: "mqtt-rx", Transport: "mqtt", SessionID: "mqtt-in",
		Topics: []ports.SubscriptionDef{{Topic: "sensors/#", QoS: 1}},
	}
	receiver.SetDecoded(&paho.Config{}, nil)
	history := &ports.StoreConfig{Type: nativestore.SQLiteKind}
	history.SetDecoded(&nativestore.SQLiteConfig{Path: historyPath}, nil)
	return &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "gobridge-single"},
		Stores:    ports.StoresConfig{ManagedSubscriptions: history},
		Sessions:  []ports.SessionDef{session},
		Receivers: []ports.ReceiverDef{receiver},
	}, mqtt
}

// seedThrough seeds through the builder the apply path would build the runtime
// with, which is the seam production uses.
func seedThrough(t *testing.T, app *App, cfg *ports.BridgeConfig) error {
	t.Helper()
	return app.seedManagedSubscriptionBaselines(t.Context(), cfg, app.newFactoryRegistry(cfg).builder)
}

func seedingApp(t *testing.T, baselines map[string][]string) *App {
	t.Helper()
	return NewApp(deployinfra.BootstrapConfig{
		BridgeID:                     "gobridge-single",
		ConfigFilePath:               filepath.Join(t.TempDir(), "bridge.yaml"),
		AdminAPIKeyParam:             "/admin",
		ManagedSubscriptionBaselines: baselines,
	}, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
}

func TestApp_SeedsManagedSubscriptionBaselinesAtBoot(t *testing.T) {
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	cfg, mqtt := durableIngressConfig(historyPath)
	app := seedingApp(t, map[string][]string{"mqtt-in": {}})

	require.NoError(t, seedThrough(t, app, cfg))
	require.NoError(t, seedThrough(t, app, cfg),
		"seeding runs on every apply, so it must be idempotent")

	identity, err := mqtt.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(t.Context(),
		&nativestore.SQLiteConfig{Path: historyPath})
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := store.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	filters, err := store.List(t.Context(), identity)
	require.NoError(t, err, "the durable identity must have a baseline the adapter can load before broker activation")
	require.Empty(t, filters, "an empty attestation records a new identity with no filters")
}

// The bootstrap document is frozen while the bridge config is live, and a task
// can boot on the empty start-empty config before the seeder has written the
// document: an attestation the boot config cannot take is skipped, not fatal.
func TestApp_SeedSkipsWhatTheBootConfigCannotTake(t *testing.T) {
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	cfg, mqtt := durableIngressConfig(historyPath)
	app := seedingApp(t, map[string][]string{"mqtt-in": {}, "ghost": {}})

	require.NoError(t, seedThrough(t, app, cfg),
		"an attested session the config does not carry is skipped")
	identity, err := mqtt.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(t.Context(),
		&nativestore.SQLiteConfig{Path: historyPath})
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := store.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	_, err = store.List(t.Context(), identity)
	require.NoError(t, err, "the session the config does carry is still seeded")

	empty := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "gobridge-single"}}
	require.NoError(t, seedThrough(t, app, empty),
		"the start-empty config names no store, so there is nothing to seed and nothing to refuse")
}

// The seeded set must be the set the runtime demands a baseline for, which
// includes a durable session that only publishes: the bridge requires its
// history once a managed-subscription store is configured, and a seed that
// skipped it would leave a session that cannot start.
func TestApp_SeedsADurablePublishOnlySessionToo(t *testing.T) {
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	cfg, _ := durableIngressConfig(historyPath)
	publisherConfig := &paho.Config{Session: paho.SessionOptions{
		BrokerURLs: []string{"tcp://broker.example:1883"}, ClientID: "gobridge-single-pub",
		ConnectTimeout: 5 * time.Second, KeepAlive: 10,
	}}
	publisher := ports.SessionDef{ID: "mqtt-pub", Transport: "mqtt", SessionMode: string(connectivity.SessionPersistent)}
	publisher.SetDecoded(publisherConfig, nil)
	cfg.Sessions = append(cfg.Sessions, publisher)
	app := seedingApp(t, map[string][]string{"mqtt-in": {}, "mqtt-pub": {}})

	require.NoError(t, seedThrough(t, app, cfg))

	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(t.Context(),
		&nativestore.SQLiteConfig{Path: historyPath})
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := store.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	identity, err := publisherConfig.DurableSessionIdentity(connectivity.SessionPersistent)
	require.NoError(t, err)
	_, err = store.List(t.Context(), identity)
	require.NoError(t, err, "a durable publish-only session must have its baseline seeded")
}

func TestApp_SeedIsANoOpWithoutBaselines(t *testing.T) {
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	cfg, _ := durableIngressConfig(historyPath)
	app := seedingApp(t, nil)

	require.NoError(t, seedThrough(t, app, cfg))
	_, err := os.Stat(historyPath)
	require.True(t, os.IsNotExist(err), "no baseline declared means the store is not even opened")
}
