package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// FileConfigSource Tests
//
// Validates FileConfigSource lifecycle and operations.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                   FileConfigSource Lifecycle                            │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  [NewConfigSource] ──▶ [Discover] ──▶ [Get/List] ──▶ [Reload] ──▶ [Close]
// │        │                    │              │            │                │
// │        ▼                    ▼              ▼            ▼                │
// │   Verify file          Load items    Lookup items   Detect changes      │
// │   Apply options        Build map     by key                              │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// createTestConfig creates a temporary config file for testing.
func createTestConfig(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test config: %v", err)
	}
	return configPath
}

// testConfig is a minimal valid configuration for testing.
const testConfig = `
bridge:
  id: test-bridge
  clusterId: test-cluster

connections:
  - id: mqtt-conn
    type: mqtt
    brokerUrls:
      - tcp://localhost:1883

pipelines:
  - id: test-pipeline
    source:
      connectionId: mqtt-conn
      topics:
        - test/#
    target:
      connectionId: sqs-conn
      topic: output

routes:
  - id: test-route
    pipelineIds:
      - test-pipeline
`

// ═══════════════════════════════════════════════════════════════════════════
// NewConfigSource Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewConfigSource validates source creation with valid file path.
func TestNewConfigSource(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	if source.Path() != configPath {
		t.Errorf("Path() = %q, want %q", source.Path(), configPath)
	}
}

// TestNewConfigSource_FileNotFound validates error when file doesn't exist.
func TestNewConfigSource_FileNotFound(t *testing.T) {
	_, err := NewConfigSource("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("NewConfigSource() expected error for nonexistent file, got nil")
	}
}

// TestNewConfigSource_WithFormat validates format override option.
func TestNewConfigSource_WithFormat(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath, WithFormat(FormatYAML))
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	// Verify format is applied by discovering (parsing should work)
	items, err := source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(items) == 0 {
		t.Error("Discover() returned no items, expected config to be parsed")
	}
}

// TestNewConfigSource_WithWatch validates watch enable option.
func TestNewConfigSource_WithWatch(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath, WithWatch(true))
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	// Discover first to initialize
	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Watch should not return error when enabled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = source.Watch(ctx)
	if err != nil {
		t.Errorf("Watch() error = %v, want nil when watch enabled", err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Discover Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_Discover validates loading and parsing config items.
func TestFileConfigSource_Discover(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	items, err := source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Should have: 1 bridge + 1 connection + 1 pipeline + 1 route = 4 items
	if len(items) != 4 {
		t.Errorf("len(items) = %d, want 4", len(items))
	}

	// Verify we can find the expected items
	itemTypes := make(map[types.ConfigItemType]int)
	for _, item := range items {
		itemTypes[item.GetType()]++
	}

	if itemTypes[types.ConfigItemType("bridge")] != 1 {
		t.Errorf("bridge items = %d, want 1", itemTypes[types.ConfigItemType("bridge")])
	}
	if itemTypes[types.ConfigItemTypeConnection] != 1 {
		t.Errorf("connection items = %d, want 1", itemTypes[types.ConfigItemTypeConnection])
	}
	if itemTypes[types.ConfigItemTypePipeline] != 1 {
		t.Errorf("pipeline items = %d, want 1", itemTypes[types.ConfigItemTypePipeline])
	}
	if itemTypes[types.ConfigItemTypeRoute] != 1 {
		t.Errorf("route items = %d, want 1", itemTypes[types.ConfigItemTypeRoute])
	}
}

// TestFileConfigSource_Discover_InvalidFile validates error on parse failure.
func TestFileConfigSource_Discover_InvalidFile(t *testing.T) {
	// Create file with invalid YAML
	configPath := createTestConfig(t, "invalid: [yaml: content")

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Discover(context.Background())
	if err == nil {
		t.Error("Discover() expected error for invalid YAML, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Get Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_Get validates retrieving a specific item.
func TestFileConfigSource_Get(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	// Must discover first
	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Get the bridge config
	item, err := source.Get(context.Background(), "bridge:test-bridge", "settings")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if item.GetPartitionKey() != "bridge:test-bridge" {
		t.Errorf("GetPartitionKey() = %q, want %q", item.GetPartitionKey(), "bridge:test-bridge")
	}
}

// TestFileConfigSource_Get_NotFound validates ErrNotFound for missing item.
func TestFileConfigSource_Get_NotFound(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	_, err = source.Get(context.Background(), "nonexistent", "key")
	if err != types.ErrNotFound {
		t.Errorf("Get() error = %v, want %v", err, types.ErrNotFound)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// List Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_List validates filtering items by partition key.
func TestFileConfigSource_List(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// List pipeline items
	items, err := source.List(context.Background(), "pipeline:")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(items) != 1 {
		t.Errorf("len(items) = %d, want 1 pipeline item", len(items))
	}
}

// TestFileConfigSource_List_Empty validates empty result for no matches.
func TestFileConfigSource_List_Empty(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	items, err := source.List(context.Background(), "nonexistent:")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0 for non-matching prefix", len(items))
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Reload Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_Reload validates change detection after reload.
func TestFileConfigSource_Reload(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write initial config
	if err := os.WriteFile(configPath, []byte(testConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Update config with new pipeline (complete rewrite with additional pipeline)
	updatedConfig := `
bridge:
  id: test-bridge
  clusterId: test-cluster

connections:
  - id: mqtt-conn
    type: mqtt
    brokerUrls:
      - tcp://localhost:1883

pipelines:
  - id: test-pipeline
    source:
      connectionId: mqtt-conn
      topics:
        - test/#
    target:
      connectionId: sqs-conn
      topic: output
  - id: new-pipeline
    source:
      connectionId: mqtt-conn
      topics:
        - new/#
    target:
      connectionId: sqs-conn
      topic: new-output

routes:
  - id: test-route
    pipelineIds:
      - test-pipeline
`
	if err := os.WriteFile(configPath, []byte(updatedConfig), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	changes, err := source.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	// Should detect changes (version changes due to new mod time + new pipeline)
	if len(changes) == 0 {
		t.Error("Reload() returned no changes, expected at least 1")
	}

	// Verify new pipeline is discoverable
	items, err := source.List(context.Background(), "pipeline:")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2 pipelines after reload", len(items))
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Watch Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_Watch_NotEnabled validates error when watch disabled.
func TestFileConfigSource_Watch_NotEnabled(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	// Create source without watch enabled (default)
	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	_, err = source.Watch(context.Background())
	if err == nil {
		t.Error("Watch() expected error when watch not enabled, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Close Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileConfigSource_Close validates clean shutdown.
func TestFileConfigSource_Close(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath, WithWatch(true))
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}

	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Start watching
	ctx, cancel := context.WithCancel(context.Background())
	_, err = source.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	cancel() // Stop watching

	// Close should not error
	if err := source.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Multiple closes should be safe
	if err := source.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}

// TestFileConfigSource_Path validates path retrieval.
func TestFileConfigSource_Path(t *testing.T) {
	configPath := createTestConfig(t, testConfig)

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	if source.Path() != configPath {
		t.Errorf("Path() = %q, want %q", source.Path(), configPath)
	}
}
