package dynamodb

import (
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// DynamoDB ConfigSource Unit Tests
//
// Tests for item parsing, key generation, and configuration defaults.
// Integration tests with LocalStack are in integration_dynamodb_test.go
// ═══════════════════════════════════════════════════════════════════════════

// TestItem_Properties validates ConfigItem implementation.
func TestItem_Properties(t *testing.T) {
	now := time.Now()
	item := &Item{
		pk:        "app/service",
		sk:        "config/main",
		itemType:  types.ConfigItemTypePipeline,
		version:   3,
		data:      map[string]any{"key": "value"},
		updatedAt: now,
	}

	if item.GetPartitionKey() != "app/service" {
		t.Errorf("expected PartitionKey app/service, got %s", item.GetPartitionKey())
	}
	if item.GetSortKey() != "config/main" {
		t.Errorf("expected SortKey config/main, got %s", item.GetSortKey())
	}
	if item.GetType() != types.ConfigItemTypePipeline {
		t.Errorf("expected Type pipeline, got %s", item.GetType())
	}
	if item.GetVersion() != 3 {
		t.Errorf("expected Version 3, got %d", item.GetVersion())
	}
	data, ok := item.GetData().(map[string]any)
	if !ok {
		t.Fatalf("expected Data to be map[string]any, got %T", item.GetData())
	}
	if data["key"] != "value" {
		t.Errorf("expected Data key=value, got %v", data["key"])
	}
	if !item.GetUpdatedAt().Equal(now) {
		t.Errorf("expected UpdatedAt %v, got %v", now, item.GetUpdatedAt())
	}
}

// TestConfig_Defaults validates default configuration values.
func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{TableName: "test-table"}
	applyDefaults(cfg)

	if cfg.PartitionKeyName != "pk" {
		t.Errorf("expected default PartitionKeyName 'pk', got %s", cfg.PartitionKeyName)
	}
	if cfg.SortKeyName != "sk" {
		t.Errorf("expected default SortKeyName 'sk', got %s", cfg.SortKeyName)
	}
	if cfg.TypeAttributeName != "type" {
		t.Errorf("expected default TypeAttributeName 'type', got %s", cfg.TypeAttributeName)
	}
	if cfg.VersionAttributeName != "version" {
		t.Errorf("expected default VersionAttributeName 'version', got %s", cfg.VersionAttributeName)
	}
	if cfg.DataAttributeName != "data" {
		t.Errorf("expected default DataAttributeName 'data', got %s", cfg.DataAttributeName)
	}
	if cfg.UpdatedAtAttributeName != "updatedAt" {
		t.Errorf("expected default UpdatedAtAttributeName 'updatedAt', got %s", cfg.UpdatedAtAttributeName)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("expected default PollInterval 30s, got %v", cfg.PollInterval)
	}
}

// TestSource_ItemKey validates composite key generation.
func TestSource_ItemKey(t *testing.T) {
	s := &Source{}

	key := s.itemKey("partition", "sort")
	if key != "partition#sort" {
		t.Errorf("expected key 'partition#sort', got %s", key)
	}

	key = s.itemKey("app/service", "config/main")
	if key != "app/service#config/main" {
		t.Errorf("expected key 'app/service#config/main', got %s", key)
	}
}

// TestSource_ParseItem validates DynamoDB item parsing.
func TestSource_ParseItem(t *testing.T) {
	s := &Source{}
	applyDefaults(&s.config)

	ddbItem := map[string]ddbtypes.AttributeValue{
		"pk":        &ddbtypes.AttributeValueMemberS{Value: "myapp"},
		"sk":        &ddbtypes.AttributeValueMemberS{Value: "main"},
		"type":      &ddbtypes.AttributeValueMemberS{Value: "json"},
		"version":   &ddbtypes.AttributeValueMemberN{Value: "5"},
		"data":      &ddbtypes.AttributeValueMemberS{Value: `{"setting":"value"}`},
		"updatedAt": &ddbtypes.AttributeValueMemberS{Value: "2024-01-15T10:30:00Z"},
	}

	item, err := s.parseItem(ddbItem)
	if err != nil {
		t.Fatalf("parseItem failed: %v", err)
	}

	if item.pk != "myapp" {
		t.Errorf("expected pk 'myapp', got %s", item.pk)
	}
	if item.sk != "main" {
		t.Errorf("expected sk 'main', got %s", item.sk)
	}
	if string(item.itemType) != "json" {
		t.Errorf("expected type 'json', got %s", item.itemType)
	}
	if item.version != 5 {
		t.Errorf("expected version 5, got %d", item.version)
	}
	// Data is parsed as any, check the map
	if data, ok := item.data.(map[string]any); ok {
		if data["setting"] != "value" {
			t.Errorf("expected data setting=value, got %v", data["setting"])
		}
	} else {
		t.Errorf("expected data to be map[string]any, got %T", item.data)
	}
}

// TestOptions validates functional options.
func TestOptions(t *testing.T) {
	s := &Source{config: Config{}}

	WithRegion("us-west-2")(s)
	if s.config.Region != "us-west-2" {
		t.Errorf("expected region 'us-west-2', got %s", s.config.Region)
	}

	WithNamespace("myapp/prod")(s)
	if s.config.Namespace != "myapp/prod" {
		t.Errorf("expected namespace 'myapp/prod', got %s", s.config.Namespace)
	}

	WithPollInterval(1 * time.Minute)(s)
	if s.config.PollInterval != 1*time.Minute {
		t.Errorf("expected poll interval 1m, got %v", s.config.PollInterval)
	}

	WithEndpoint("http://localhost:4566")(s)
	if s.config.Endpoint != "http://localhost:4566" {
		t.Errorf("expected endpoint 'http://localhost:4566', got %s", s.config.Endpoint)
	}
}
