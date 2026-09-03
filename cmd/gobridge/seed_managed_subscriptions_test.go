package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The reference binary's -seed-managed-subscriptions flag is how a deployment
// without the AWS profile attests a durable MQTT session's baseline: the
// Kubernetes profile runs it from an init container, an operator runs it once
// by hand. Each value is `session-id` (an empty baseline) or
// `session-id=filter,filter` (the exact filters the broker session already holds).

func TestParseManagedSubscriptionBaselines_EmptyAndListedFilters(t *testing.T) {
	got, err := parseManagedSubscriptionBaselines([]string{
		"mqtt-conn",
		"legacy=orders/legacy/#,$share/group/orders/#",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, ok := got["mqtt-conn"]; !ok || len(v) != 0 {
		t.Fatalf("mqtt-conn = %v, %v; want present and empty", v, ok)
	}
	if v := got["legacy"]; len(v) != 2 || v[0] != "orders/legacy/#" || v[1] != "$share/group/orders/#" {
		t.Fatalf("legacy = %v", v)
	}
}

func TestParseManagedSubscriptionBaselines_RejectsMalformedValues(t *testing.T) {
	for _, bad := range [][]string{
		{""},
		{"=a/#"},
		{"s=a/#,"},
		{"s", "s"},
	} {
		if _, err := parseManagedSubscriptionBaselines(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// writeSeedConfig writes a persistent MQTT config whose managed-subscription
// store lives under dir, and returns the config path and the store path.
func writeSeedConfig(t *testing.T, dir string) (string, string) {
	t.Helper()
	storePath := filepath.Join(dir, "state", "managed-subscriptions.db")
	cfgPath := filepath.Join(dir, "bridge.yaml")
	cfg := `bridge:
  id: seed-test
sessions:
  - id: mqtt-conn
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: tcp://127.0.0.1:1
        client_id: seed-test-01
        clean_start: false
        session_expiry_interval: 3600
stores:
  managed_subscriptions:
    type: sqlite
    options:
      path: ` + storePath + `
receivers:
  - id: sensor-in
    session_id: mqtt-conn
    topics:
      - topic: "sensors/#"
        qos: 1
senders:
  - id: sensor-out
    session_id: mqtt-conn
    options:
      sender:
        default_topic: archive/sensors
bindings:
  - id: to-archive
    sender_id: sensor-out
    address: archive/sensors
routes:
  - id: forward
    receiver_id: sensor-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-archive]
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storePath
}

func TestSeedManagedSubscriptions_EstablishesBaselineTheSessionWillLoad(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, storePath := writeSeedConfig(t, dir)

	reg := ports.NewRegistry()
	if err := errors.Join(paho.Register(reg), nativestore.Register(reg)); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := fileconfig.NewSource(cfgPath, reg)

	if err := seedManagedSubscriptions(context.Background(), loader, map[string][]string{"mqtt-conn": nil}, logger); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The proof is the store's own answer: the identity the session derives
	// now has a baseline, so List returns an empty history instead of not-found.
	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := cfg.Sessions[0].Config.(ports.DurableSessionIdentityConfig).DurableSessionIdentity(connectivity.SessionPersistent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(context.Background(), nativestore.SQLiteConfig{Path: storePath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	history, err := store.List(context.Background(), identity)
	if err != nil {
		t.Fatalf("List after seed: %v (want an established, empty baseline)", err)
	}
	if len(history) != 0 {
		t.Fatalf("history = %v, want empty", history)
	}

	// Seeding again is a no-op, so a deployment may run it on every start.
	if err := seedManagedSubscriptions(context.Background(), loader, map[string][]string{"mqtt-conn": nil}, logger); err != nil {
		t.Fatalf("second seed: %v", err)
	}
}

func TestSeedManagedSubscriptions_RejectsUnknownSession(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, storePath := writeSeedConfig(t, dir)
	reg := ports.NewRegistry()
	if err := errors.Join(paho.Register(reg), nativestore.Register(reg)); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	err = seedManagedSubscriptions(context.Background(), fileconfig.NewSource(cfgPath, reg), map[string][]string{"nope": nil}, logger)
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("error = %v, want shared.ErrInvalidConfig", err)
	}
	if _, statErr := os.Stat(storePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a rejected seed must not create the store, stat err = %v", statErr)
	}
}
