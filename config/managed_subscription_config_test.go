package config

import (
	"github.com/mariotoffia/gobridge/ports"
	"testing"
)

type managedConfigPlugin struct {
	Table string `json:"table"`
}

func (managedConfigPlugin) Kind() string    { return "managed-test" }
func (managedConfigPlugin) Validate() error { return nil }

func TestManagedSubscriptionStoreMergeAndFingerprint(t *testing.T) {
	baseStore := &ports.StoreConfig{Type: "managed-test", Config: managedConfigPlugin{Table: "a"}}
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge"}, Stores: ports.StoresConfig{ManagedSubscriptions: baseStore}}
	merged, err := DefaultMerge(base, &ports.BridgeConfig{})
	if err != nil {
		t.Fatalf("DefaultMerge: %v", err)
	}
	if merged.Stores.ManagedSubscriptions == nil || merged.Stores.ManagedSubscriptions == baseStore {
		t.Fatal("managed store was not retained as an isolated clone")
	}
	a, err := configFingerprint(base)
	if err != nil {
		t.Fatalf("fingerprint a: %v", err)
	}
	changed := *base
	changed.Stores.ManagedSubscriptions = &ports.StoreConfig{Type: "managed-test", Config: managedConfigPlugin{Table: "b"}}
	b, err := configFingerprint(&changed)
	if err != nil {
		t.Fatalf("fingerprint b: %v", err)
	}
	if a == b {
		t.Fatal("managed store options must participate in config fingerprint")
	}
}
