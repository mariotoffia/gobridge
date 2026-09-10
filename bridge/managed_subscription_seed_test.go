package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A durable MQTT session refuses to start until its managed-subscription
// baseline exists (the paho adapter loads the exact filter history before it
// opens the broker connection, and a missing baseline is not an empty set). The
// AWS profile seeds that row at deploy time; every other composition root needs
// the same operation, and SeedManagedSubscriptionBaselines is it.

// seedRecordingStore records every Remember call and whether it was closed.
type seedRecordingStore struct {
	mu        sync.Mutex
	remembers map[string][]string
	closes    int
}

func (s *seedRecordingStore) List(context.Context, string) ([]string, error) { return nil, nil }
func (s *seedRecordingStore) Remember(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remembers == nil {
		s.remembers = map[string][]string{}
	}
	s.remembers[identity] = append([]string(nil), filters...)
	return nil
}
func (s *seedRecordingStore) Forget(context.Context, string, []string) error { return nil }
func (s *seedRecordingStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

// seedRecordingFactory serves the managed store and fails loudly if the seed
// path asks for any other role: seeding must not open the lease, outbox or DLQ.
type seedRecordingFactory struct {
	store      *seedRecordingStore
	otherRoles int
}

func (f *seedRecordingFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	f.otherRoles++
	return nil, errors.New("seed must not open the lease store")
}
func (f *seedRecordingFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	f.otherRoles++
	return nil, errors.New("seed must not open the outbox store")
}
func (f *seedRecordingFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	f.otherRoles++
	return nil, errors.New("seed must not open the DLQ store")
}
func (f *seedRecordingFactory) NewManagedSubscriptionStore(context.Context, ports.PluginConfig) (ports.ManagedSubscriptionStore, error) {
	return f.store, nil
}

func seedTestConfig(mode string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "seed-test"},
		Stores: ports.StoresConfig{
			Lease:                &ports.StoreConfig{Type: "recording"},
			Outbox:               &ports.StoreConfig{Type: "recording"},
			DLQ:                  &ports.StoreConfig{Type: "recording"},
			ManagedSubscriptions: &ports.StoreConfig{Type: "recording"},
		},
		Sessions: []ports.SessionDef{
			{ID: "durable", Transport: "mqtt", SessionMode: mode, Config: managedIdentityConfig{}},
			{ID: "ephemeral", Transport: "mqtt", Config: managedIdentityConfig{}},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "durable", Topics: []ports.SubscriptionDef{{Topic: "sensors/#"}}}},
	}
}

func TestSeedManagedSubscriptionBaselines_EstablishesEmptyBaselineForPersistentSession(t *testing.T) {
	store := &seedRecordingStore{}
	factory := &seedRecordingFactory{store: store}
	b := NewBuilder(seedTestConfig("persistent")).RegisterStoreFactory("recording", factory)

	err := b.SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{"durable": nil})
	if err != nil {
		t.Fatalf("SeedManagedSubscriptionBaselines: %v", err)
	}
	got, ok := store.remembers["safe-durable-fingerprint"]
	if !ok {
		t.Fatalf("baseline not remembered under the session's durable identity; remembers=%v", store.remembers)
	}
	if len(got) != 0 {
		t.Fatalf("empty attestation must remember no filters, got %v", got)
	}
	if factory.otherRoles != 0 {
		t.Fatalf("seed opened %d non-managed store roles; it must open only the managed store", factory.otherRoles)
	}
	if store.closes != 1 {
		t.Fatalf("managed store Close count = %d, want 1 (the seed owns the handle it opened)", store.closes)
	}
}

func TestSeedManagedSubscriptionBaselines_RemembersFiltersForExclusiveSession(t *testing.T) {
	store := &seedRecordingStore{}
	b := NewBuilder(seedTestConfig("exclusive")).RegisterStoreFactory("recording", &seedRecordingFactory{store: store})

	err := b.SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{
		"durable": {"orders/legacy/#", "$share/group/orders/#"},
	})
	if err != nil {
		t.Fatalf("SeedManagedSubscriptionBaselines: %v", err)
	}
	got := store.remembers["safe-durable-fingerprint"]
	if len(got) != 2 || got[0] != "orders/legacy/#" || got[1] != "$share/group/orders/#" {
		t.Fatalf("remembered filters = %v", got)
	}
}

func TestSeedManagedSubscriptionBaselines_RejectsWhatItCannotSeed(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *ports.BridgeConfig
		baselines map[string][]string
	}{
		{"unknown session", seedTestConfig("persistent"), map[string][]string{"nope": nil}},
		{"session is not durable", seedTestConfig("persistent"), map[string][]string{"ephemeral": nil}},
		{"empty filter", seedTestConfig("persistent"), map[string][]string{"durable": {""}}},
		{"nothing to seed", seedTestConfig("persistent"), nil},
		{"no managed store configured", func() *ports.BridgeConfig {
			cfg := seedTestConfig("persistent")
			cfg.Stores.ManagedSubscriptions = nil
			return cfg
		}(), map[string][]string{"durable": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &seedRecordingStore{}
			b := NewBuilder(tc.cfg).RegisterStoreFactory("recording", &seedRecordingFactory{store: store})
			err := b.SeedManagedSubscriptionBaselines(t.Context(), tc.baselines)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("error = %v, want shared.ErrInvalidConfig", err)
			}
			if len(store.remembers) != 0 {
				t.Fatalf("a rejected seed must remember nothing, got %v", store.remembers)
			}
		})
	}
}
