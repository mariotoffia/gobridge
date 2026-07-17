package parser

import (
	"encoding/json"
	"github.com/mariotoffia/gobridge/ports"
	"testing"
)

type managedMarshalConfig struct {
	TableName string `json:"table_name" yaml:"table_name"`
}

func (managedMarshalConfig) Kind() string    { return "dynamodb" }
func (managedMarshalConfig) Validate() error { return nil }

func TestMarshalManagedSubscriptionStoreOptions(t *testing.T) {
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge"}, Stores: ports.StoresConfig{ManagedSubscriptions: &ports.StoreConfig{Type: "dynamodb", Config: managedMarshalConfig{TableName: "managed-table"}}}}
	data, err := MarshalBridgeConfigJSON(cfg)
	if err != nil {
		t.Fatalf("MarshalBridgeConfigJSON: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	stores := wire["stores"].(map[string]any)
	managed := stores["managed_subscriptions"].(map[string]any)
	options := managed["options"].(map[string]any)
	if options["table_name"] != "managed-table" {
		t.Fatalf("options = %v", options)
	}
}
