package bridge

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

type managedIdentityConfig struct{}

func (managedIdentityConfig) Kind() string    { return "mqtt" }
func (managedIdentityConfig) Validate() error { return nil }
func (managedIdentityConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return "safe-durable-fingerprint", nil
}
func (managedIdentityConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	return []string{"safe-domain"}, nil
}

type managedSpecStore struct{}

func (managedSpecStore) List(context.Context, string) ([]string, error)   { return nil, nil }
func (managedSpecStore) Remember(context.Context, string, []string) error { return nil }
func (managedSpecStore) Forget(context.Context, string, []string) error   { return nil }

func TestSessionSpecManagedSubscriptionsForDurableMQTT(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions:  []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent", Config: managedIdentityConfig{}}},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "mqtt-sess", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}
	spec, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, managedSpecStore{})
	if err != nil {
		t.Fatalf("sessionSpecWithManagedSubscriptions: %v", err)
	}
	if !spec.ManagedSubscriptionsRequired {
		t.Fatal("durable MQTT subscriptions must require history")
	}
	if spec.ManagedSubscriptionStore == nil {
		t.Fatal("managed store was not injected")
	}
	if spec.ManagedSubscriptionIdentity != "safe-durable-fingerprint" {
		t.Fatalf("identity = %q", spec.ManagedSubscriptionIdentity)
	}
}

func TestSessionSpecManagedSubscriptionsForFullyQualifiedMQTTAlias(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions:  []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt.paho", SessionMode: "exclusive", Config: managedIdentityConfig{}}},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "mqtt-sess", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}
	spec, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, managedSpecStore{})
	if err != nil {
		t.Fatalf("sessionSpecWithManagedSubscriptions: %v", err)
	}
	if !spec.ManagedSubscriptionsRequired || spec.ManagedSubscriptionStore == nil {
		t.Fatal("mqtt.paho alias must preserve durable managed-subscription history")
	}
	if !requiresManagedSubscriptionStore(cfg) {
		t.Fatal("mqtt.paho alias must require the managed-subscription store")
	}
}

func TestSessionSpecEphemeralDoesNotOwnManagedHistory(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions:  []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "ephemeral", Config: managedIdentityConfig{}}},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "mqtt-sess", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}
	spec, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, managedSpecStore{})
	if err != nil {
		t.Fatalf("sessionSpecWithManagedSubscriptions: %v", err)
	}
	if spec.ManagedSubscriptionsRequired || spec.ManagedSubscriptionStore != nil || spec.ManagedSubscriptionIdentity != "" {
		t.Fatalf("ephemeral spec unexpectedly owns managed history: %+v", spec)
	}
}

func TestSessionSpecDurableEmptyPlanRetainsManagedStoreForCleanup(t *testing.T) {
	cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent", Config: managedIdentityConfig{}}}}
	spec, err := sessionSpecWithManagedSubscriptions(cfg.Sessions[0], cfg, managedSpecStore{})
	if err != nil {
		t.Fatalf("sessionSpecWithManagedSubscriptions: %v", err)
	}
	if !spec.ManagedSubscriptionsRequired || spec.ManagedSubscriptionStore == nil {
		t.Fatal("empty replacement plan must retain history for exact stale cleanup")
	}
}

func TestPlanRejectsDurableMQTTSubscriptionsWithoutManagedStore(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "bridge"},
		Sessions:  []ports.SessionDef{{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent", Config: managedIdentityConfig{}}},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "mqtt-sess", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if plan != nil {
		plan.Close()
	}
	if err == nil {
		t.Fatal("Plan must fail before broker activation when managed history is absent")
	}
}

type closableManagedSpecStore struct {
	managedSpecStore
	closes atomic.Int32
}

func (s *closableManagedSpecStore) Close() error { s.closes.Add(1); return nil }

type managedOnlySpecFactory struct{ store *closableManagedSpecStore }

func (*managedOnlySpecFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return nil, nil
}
func (*managedOnlySpecFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return nil, nil
}
func (*managedOnlySpecFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}
func (f *managedOnlySpecFactory) NewManagedSubscriptionStore(context.Context, ports.PluginConfig) (ports.ManagedSubscriptionStore, error) {
	return f.store, nil
}

func TestCompleteSessionBuildFailureClosesManagedStore(t *testing.T) {
	store := &closableManagedSpecStore{}
	cfg := &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "bridge"},
		Stores:   ports.StoresConfig{ManagedSubscriptions: &ports.StoreConfig{Type: "managed-only"}},
		Sessions: []ports.SessionDef{{ID: "session", Transport: "unregistered"}},
		Senders:  []ports.SenderDef{{ID: "sender", SessionID: "session"}},
	}
	builder := NewBuilder(cfg).RegisterStoreFactory("managed-only", &managedOnlySpecFactory{store: store})
	if _, err := builder.Build(t.Context()); err == nil {
		t.Fatal("Build must fail for the unregistered transport")
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("managed store Close count = %d, want 1", got)
	}
}
