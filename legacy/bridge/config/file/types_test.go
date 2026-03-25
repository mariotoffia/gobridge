package file

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// fileConfigItem Interface Tests
//
// Validates that fileConfigItem correctly implements types.ConfigItem.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                      fileConfigItem Structure                           │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  partitionKey ──▶ GetPartitionKey()                                     │
// │  sortKey      ──▶ GetSortKey()                                          │
// │  itemType     ──▶ GetType()                                             │
// │  version      ──▶ GetVersion()                                          │
// │  data         ──▶ GetData()                                             │
// │  updatedAt    ──▶ GetUpdatedAt()                                        │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigItem_GetPartitionKey validates partition key retrieval.
func TestFileConfigItem_GetPartitionKey(t *testing.T) {
	item := &fileConfigItem{
		partitionKey: "pipeline:test-pipeline",
	}

	if got := item.GetPartitionKey(); got != "pipeline:test-pipeline" {
		t.Errorf("GetPartitionKey() = %q, want %q", got, "pipeline:test-pipeline")
	}
}

// TestFileConfigItem_GetSortKey validates sort key retrieval.
func TestFileConfigItem_GetSortKey(t *testing.T) {
	item := &fileConfigItem{
		sortKey: "settings",
	}

	if got := item.GetSortKey(); got != "settings" {
		t.Errorf("GetSortKey() = %q, want %q", got, "settings")
	}
}

// TestFileConfigItem_GetType validates type retrieval.
func TestFileConfigItem_GetType(t *testing.T) {
	item := &fileConfigItem{
		itemType: types.ConfigItemTypePipeline,
	}

	if got := item.GetType(); got != types.ConfigItemTypePipeline {
		t.Errorf("GetType() = %v, want %v", got, types.ConfigItemTypePipeline)
	}
}

// TestFileConfigItem_GetVersion validates version retrieval.
func TestFileConfigItem_GetVersion(t *testing.T) {
	item := &fileConfigItem{
		version: 1234567890,
	}

	if got := item.GetVersion(); got != 1234567890 {
		t.Errorf("GetVersion() = %d, want %d", got, 1234567890)
	}
}

// TestFileConfigItem_GetData validates data retrieval.
func TestFileConfigItem_GetData(t *testing.T) {
	data := PipelineConfig{
		ID: "test-pipeline",
	}
	item := &fileConfigItem{
		data: data,
	}

	got, ok := item.GetData().(PipelineConfig)
	if !ok {
		t.Fatalf("GetData() returned unexpected type: %T", item.GetData())
	}
	if got.ID != "test-pipeline" {
		t.Errorf("GetData().ID = %q, want %q", got.ID, "test-pipeline")
	}
}

// TestFileConfigItem_GetUpdatedAt validates timestamp retrieval.
func TestFileConfigItem_GetUpdatedAt(t *testing.T) {
	now := time.Now()
	item := &fileConfigItem{
		updatedAt: now,
	}

	if got := item.GetUpdatedAt(); !got.Equal(now) {
		t.Errorf("GetUpdatedAt() = %v, want %v", got, now)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Format Constants Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFormatConstants validates format string constants.
func TestFormatConstants(t *testing.T) {
	tests := []struct {
		name     string
		format   Format
		expected string
	}{
		{"FormatAuto", FormatAuto, "auto"},
		{"FormatYAML", FormatYAML, "yaml"},
		{"FormatJSON", FormatJSON, "json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.format) != tt.expected {
				t.Errorf("%s = %q, want %q", tt.name, tt.format, tt.expected)
			}
		})
	}
}
