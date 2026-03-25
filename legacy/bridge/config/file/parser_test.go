package file

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// Parser Tests
//
// Validates YAML/JSON parsing, format detection, and config conversion.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                           Parser Flow                                    │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  Input ([]byte) ──▶ [Parse] ──▶ FileConfig ──▶ [ToConfigItems] ──▶ []ConfigItem
// │                         │                                                │
// │                    ┌────┴────┐                                           │
// │                    │ Format  │                                           │
// │                    │YAML/JSON│                                           │
// │                    └─────────┘                                           │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// sampleYAML is a valid YAML configuration for testing.
const sampleYAML = `
bridge:
  id: test-bridge
  clusterId: test-cluster
  shutdownTimeout: "30s"
  drainTimeout: "10s"

connections:
  - id: mqtt-conn
    type: mqtt
    brokerUrls:
      - tcp://localhost:1883
    clientId: test-client

pipelines:
  - id: mqtt-to-sqs
    source:
      connectionId: mqtt-conn
      topics:
        - sensors/#
    target:
      connectionId: sqs-conn
      topic: output-queue

routes:
  - id: main-route
    pipelineIds:
      - mqtt-to-sqs
`

// sampleJSON is a valid JSON configuration for testing.
const sampleJSON = `{
  "bridge": {
    "id": "test-bridge",
    "clusterId": "test-cluster",
    "shutdownTimeout": "30s",
    "drainTimeout": "10s"
  },
  "connections": [
    {
      "id": "mqtt-conn",
      "type": "mqtt",
      "brokerUrls": ["tcp://localhost:1883"],
      "clientId": "test-client"
    }
  ],
  "pipelines": [
    {
      "id": "mqtt-to-sqs",
      "source": {
        "connectionId": "mqtt-conn",
        "topics": ["sensors/#"]
      },
      "target": {
        "connectionId": "sqs-conn",
        "topic": "output-queue"
      }
    }
  ],
  "routes": [
    {
      "id": "main-route",
      "pipelineIds": ["mqtt-to-sqs"]
    }
  ]
}`

// TestParser_ParseYAML validates parsing valid YAML configuration.
func TestParser_ParseYAML(t *testing.T) {
	parser := NewParser(FormatYAML)

	config, err := parser.Parse([]byte(sampleYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	// Verify bridge section
	if config.Bridge.ID != "test-bridge" {
		t.Errorf("Bridge.ID = %q, want %q", config.Bridge.ID, "test-bridge")
	}
	if config.Bridge.ClusterID != "test-cluster" {
		t.Errorf("Bridge.ClusterID = %q, want %q", config.Bridge.ClusterID, "test-cluster")
	}

	// Verify connections
	if len(config.Connections) != 1 {
		t.Fatalf("len(Connections) = %d, want 1", len(config.Connections))
	}
	if config.Connections[0].ID != "mqtt-conn" {
		t.Errorf("Connections[0].ID = %q, want %q", config.Connections[0].ID, "mqtt-conn")
	}

	// Verify pipelines
	if len(config.Pipelines) != 1 {
		t.Fatalf("len(Pipelines) = %d, want 1", len(config.Pipelines))
	}
	if config.Pipelines[0].ID != "mqtt-to-sqs" {
		t.Errorf("Pipelines[0].ID = %q, want %q", config.Pipelines[0].ID, "mqtt-to-sqs")
	}

	// Verify routes
	if len(config.Routes) != 1 {
		t.Fatalf("len(Routes) = %d, want 1", len(config.Routes))
	}
	if config.Routes[0].ID != "main-route" {
		t.Errorf("Routes[0].ID = %q, want %q", config.Routes[0].ID, "main-route")
	}
}

// TestParser_ParseJSON validates parsing valid JSON configuration.
func TestParser_ParseJSON(t *testing.T) {
	parser := NewParser(FormatJSON)

	config, err := parser.Parse([]byte(sampleJSON), FormatJSON)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	// Verify bridge section
	if config.Bridge.ID != "test-bridge" {
		t.Errorf("Bridge.ID = %q, want %q", config.Bridge.ID, "test-bridge")
	}

	// Verify connections
	if len(config.Connections) != 1 {
		t.Fatalf("len(Connections) = %d, want 1", len(config.Connections))
	}
	if config.Connections[0].Type != "mqtt" {
		t.Errorf("Connections[0].Type = %q, want %q", config.Connections[0].Type, "mqtt")
	}

	// Verify pipelines
	if len(config.Pipelines) != 1 {
		t.Fatalf("len(Pipelines) = %d, want 1", len(config.Pipelines))
	}

	// Verify routes
	if len(config.Routes) != 1 {
		t.Fatalf("len(Routes) = %d, want 1", len(config.Routes))
	}
}

// TestParser_ParseYAML_Invalid validates error handling for malformed YAML.
func TestParser_ParseYAML_Invalid(t *testing.T) {
	parser := NewParser(FormatYAML)

	invalidYAML := []byte(`
bridge:
  id: test
  invalid: [unclosed
`)

	_, err := parser.Parse(invalidYAML, FormatYAML)
	if err == nil {
		t.Error("Parse() expected error for invalid YAML, got nil")
	}
}

// TestParser_ParseJSON_Invalid validates error handling for malformed JSON.
func TestParser_ParseJSON_Invalid(t *testing.T) {
	parser := NewParser(FormatJSON)

	invalidJSON := []byte(`{"bridge": {"id": "test", invalid}}`)

	_, err := parser.Parse(invalidJSON, FormatJSON)
	if err == nil {
		t.Error("Parse() expected error for invalid JSON, got nil")
	}
}

// TestParser_Parse_UnsupportedFormat validates error for unknown format.
func TestParser_Parse_UnsupportedFormat(t *testing.T) {
	parser := NewParser(FormatAuto)

	_, err := parser.Parse([]byte("data"), Format("xml"))
	if err == nil {
		t.Error("Parse() expected error for unsupported format, got nil")
	}
}

// TestParser_ParseFile validates file reading and parsing.
func TestParser_ParseFile(t *testing.T) {
	// Create temp YAML file
	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(sampleYAML), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	parser := NewParser(FormatAuto)
	config, err := parser.ParseFile(yamlPath)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	if config.Bridge.ID != "test-bridge" {
		t.Errorf("Bridge.ID = %q, want %q", config.Bridge.ID, "test-bridge")
	}
}

// TestParser_ParseFile_NotFound validates error when file doesn't exist.
func TestParser_ParseFile_NotFound(t *testing.T) {
	parser := NewParser(FormatAuto)

	_, err := parser.ParseFile("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("ParseFile() expected error for nonexistent file, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Format Detection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDetectFormat validates format auto-detection from file extensions.
func TestDetectFormat(t *testing.T) {
	tests := []struct {
		path     string
		expected Format
	}{
		{"config.yaml", FormatYAML},
		{"config.YAML", FormatYAML},
		{"config.yml", FormatYAML},
		{"config.YML", FormatYAML},
		{"config.json", FormatJSON},
		{"config.JSON", FormatJSON},
		{"/path/to/config.yaml", FormatYAML},
		{"/path/to/config.json", FormatJSON},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := detectFormat(tt.path)
			if got != tt.expected {
				t.Errorf("detectFormat(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}

// TestDetectFormat_Unknown validates default to YAML for unknown extensions.
func TestDetectFormat_Unknown(t *testing.T) {
	tests := []string{
		"config.txt",
		"config.xml",
		"config",
		"config.",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			got := detectFormat(path)
			if got != FormatYAML {
				t.Errorf("detectFormat(%q) = %v, want %v (default)", path, got, FormatYAML)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ToConfigItems Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestToConfigItems_Bridge validates bridge config item conversion.
func TestToConfigItems_Bridge(t *testing.T) {
	config := &FileConfig{
		Bridge: BridgeSection{
			ID:        "test-bridge",
			ClusterID: "test-cluster",
		},
	}
	modTime := time.Now()

	items := ToConfigItems(config, modTime)

	// Should have 1 bridge item
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	item := items[0]
	if item.GetPartitionKey() != "bridge:test-bridge" {
		t.Errorf("GetPartitionKey() = %q, want %q", item.GetPartitionKey(), "bridge:test-bridge")
	}
	if item.GetSortKey() != "settings" {
		t.Errorf("GetSortKey() = %q, want %q", item.GetSortKey(), "settings")
	}
	if item.GetType() != types.ConfigItemType("bridge") {
		t.Errorf("GetType() = %v, want %v", item.GetType(), types.ConfigItemType("bridge"))
	}
}

// TestToConfigItems_Connections validates connection config item conversion.
func TestToConfigItems_Connections(t *testing.T) {
	config := &FileConfig{
		Connections: []ConnectionConfig{
			{ID: "conn-1", Type: "mqtt"},
			{ID: "conn-2", Type: "sqs"},
		},
	}
	modTime := time.Now()

	items := ToConfigItems(config, modTime)

	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	// Verify first connection
	if items[0].GetPartitionKey() != "connection:conn-1" {
		t.Errorf("items[0].GetPartitionKey() = %q, want %q", items[0].GetPartitionKey(), "connection:conn-1")
	}
	if items[0].GetType() != types.ConfigItemTypeConnection {
		t.Errorf("items[0].GetType() = %v, want %v", items[0].GetType(), types.ConfigItemTypeConnection)
	}

	// Verify second connection
	if items[1].GetPartitionKey() != "connection:conn-2" {
		t.Errorf("items[1].GetPartitionKey() = %q, want %q", items[1].GetPartitionKey(), "connection:conn-2")
	}
}

// TestToConfigItems_Pipelines validates pipeline config item conversion.
func TestToConfigItems_Pipelines(t *testing.T) {
	config := &FileConfig{
		Pipelines: []PipelineConfig{
			{ID: "pipeline-1"},
			{ID: "pipeline-2"},
		},
	}
	modTime := time.Now()

	items := ToConfigItems(config, modTime)

	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	if items[0].GetPartitionKey() != "pipeline:pipeline-1" {
		t.Errorf("items[0].GetPartitionKey() = %q, want %q", items[0].GetPartitionKey(), "pipeline:pipeline-1")
	}
	if items[0].GetType() != types.ConfigItemTypePipeline {
		t.Errorf("items[0].GetType() = %v, want %v", items[0].GetType(), types.ConfigItemTypePipeline)
	}
}

// TestToConfigItems_Routes validates route config item conversion.
func TestToConfigItems_Routes(t *testing.T) {
	config := &FileConfig{
		Routes: []RouteConfig{
			{ID: "route-1", PipelineIDs: []string{"p1", "p2"}},
		},
	}
	modTime := time.Now()

	items := ToConfigItems(config, modTime)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	if items[0].GetPartitionKey() != "route:route-1" {
		t.Errorf("items[0].GetPartitionKey() = %q, want %q", items[0].GetPartitionKey(), "route:route-1")
	}
	if items[0].GetType() != types.ConfigItemTypeRoute {
		t.Errorf("items[0].GetType() = %v, want %v", items[0].GetType(), types.ConfigItemTypeRoute)
	}
}

// TestToConfigItems_Empty validates handling of empty configuration.
func TestToConfigItems_Empty(t *testing.T) {
	config := &FileConfig{}
	modTime := time.Now()

	items := ToConfigItems(config, modTime)

	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0 for empty config", len(items))
	}
}

// TestToConfigItems_Version validates version assignment from mod time.
func TestToConfigItems_Version(t *testing.T) {
	config := &FileConfig{
		Bridge: BridgeSection{ID: "test"},
	}
	modTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	expectedVersion := modTime.UnixNano()

	items := ToConfigItems(config, modTime)

	if items[0].GetVersion() != expectedVersion {
		t.Errorf("GetVersion() = %d, want %d", items[0].GetVersion(), expectedVersion)
	}
	if !items[0].GetUpdatedAt().Equal(modTime) {
		t.Errorf("GetUpdatedAt() = %v, want %v", items[0].GetUpdatedAt(), modTime)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// itemKey Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestItemKey validates unique key generation.
func TestItemKey(t *testing.T) {
	item := &fileConfigItem{
		partitionKey: "pipeline:test",
		sortKey:      "settings",
	}

	got := itemKey(item)
	expected := "pipeline:test:settings"

	if got != expected {
		t.Errorf("itemKey() = %q, want %q", got, expected)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ComputeChanges Tests
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                       Change Detection                                   │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  oldItems ──┬──▶ Compare by key ──┬──▶ Add (in new, not old)            │
// │  newItems ──┘                     ├──▶ Update (version changed)         │
// │                                   └──▶ Delete (in old, not new)         │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestComputeChanges_Add validates detection of added items.
func TestComputeChanges_Add(t *testing.T) {
	oldItems := []types.ConfigItem{}
	newItems := []types.ConfigItem{
		&fileConfigItem{
			partitionKey: "pipeline:new",
			sortKey:      "settings",
			version:      1,
		},
	}

	changes := ComputeChanges(oldItems, newItems)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].Type != types.ConfigChangeAdd {
		t.Errorf("changes[0].Type = %v, want %v", changes[0].Type, types.ConfigChangeAdd)
	}
	if changes[0].Item.GetPartitionKey() != "pipeline:new" {
		t.Errorf("changes[0].Item.GetPartitionKey() = %q, want %q", changes[0].Item.GetPartitionKey(), "pipeline:new")
	}
}

// TestComputeChanges_Update validates detection of updated items.
func TestComputeChanges_Update(t *testing.T) {
	oldItems := []types.ConfigItem{
		&fileConfigItem{
			partitionKey: "pipeline:existing",
			sortKey:      "settings",
			version:      1,
		},
	}
	newItems := []types.ConfigItem{
		&fileConfigItem{
			partitionKey: "pipeline:existing",
			sortKey:      "settings",
			version:      2, // Version changed
		},
	}

	changes := ComputeChanges(oldItems, newItems)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].Type != types.ConfigChangeUpdate {
		t.Errorf("changes[0].Type = %v, want %v", changes[0].Type, types.ConfigChangeUpdate)
	}
}

// TestComputeChanges_Delete validates detection of deleted items.
func TestComputeChanges_Delete(t *testing.T) {
	oldItems := []types.ConfigItem{
		&fileConfigItem{
			partitionKey: "pipeline:deleted",
			sortKey:      "settings",
			version:      1,
		},
	}
	newItems := []types.ConfigItem{}

	changes := ComputeChanges(oldItems, newItems)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].Type != types.ConfigChangeDelete {
		t.Errorf("changes[0].Type = %v, want %v", changes[0].Type, types.ConfigChangeDelete)
	}
	if changes[0].Item.GetPartitionKey() != "pipeline:deleted" {
		t.Errorf("changes[0].Item.GetPartitionKey() = %q, want %q", changes[0].Item.GetPartitionKey(), "pipeline:deleted")
	}
}

// TestComputeChanges_NoChange validates no changes when items identical.
func TestComputeChanges_NoChange(t *testing.T) {
	items := []types.ConfigItem{
		&fileConfigItem{
			partitionKey: "pipeline:unchanged",
			sortKey:      "settings",
			version:      1,
		},
	}

	changes := ComputeChanges(items, items)

	if len(changes) != 0 {
		t.Errorf("len(changes) = %d, want 0 for identical items", len(changes))
	}
}

// TestComputeChanges_Mixed validates combination of add/update/delete.
func TestComputeChanges_Mixed(t *testing.T) {
	oldItems := []types.ConfigItem{
		&fileConfigItem{partitionKey: "pipeline:updated", sortKey: "s", version: 1},
		&fileConfigItem{partitionKey: "pipeline:deleted", sortKey: "s", version: 1},
		&fileConfigItem{partitionKey: "pipeline:unchanged", sortKey: "s", version: 1},
	}
	newItems := []types.ConfigItem{
		&fileConfigItem{partitionKey: "pipeline:updated", sortKey: "s", version: 2},   // Updated
		&fileConfigItem{partitionKey: "pipeline:unchanged", sortKey: "s", version: 1}, // Unchanged
		&fileConfigItem{partitionKey: "pipeline:added", sortKey: "s", version: 1},     // Added
	}

	changes := ComputeChanges(oldItems, newItems)

	// Should have 3 changes: 1 add, 1 update, 1 delete
	if len(changes) != 3 {
		t.Fatalf("len(changes) = %d, want 3", len(changes))
	}

	// Count by type
	counts := make(map[types.ConfigChangeType]int)
	for _, c := range changes {
		counts[c.Type]++
	}

	if counts[types.ConfigChangeAdd] != 1 {
		t.Errorf("add count = %d, want 1", counts[types.ConfigChangeAdd])
	}
	if counts[types.ConfigChangeUpdate] != 1 {
		t.Errorf("update count = %d, want 1", counts[types.ConfigChangeUpdate])
	}
	if counts[types.ConfigChangeDelete] != 1 {
		t.Errorf("delete count = %d, want 1", counts[types.ConfigChangeDelete])
	}
}

// TestComputeChanges_Timestamp validates timestamp is set.
func TestComputeChanges_Timestamp(t *testing.T) {
	before := time.Now()
	changes := ComputeChanges(
		[]types.ConfigItem{},
		[]types.ConfigItem{&fileConfigItem{partitionKey: "p", sortKey: "s", version: 1}},
	)
	after := time.Now()

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}

	ts := changes[0].Timestamp
	if ts.Before(before) || ts.After(after) {
		t.Errorf("Timestamp %v not between %v and %v", ts, before, after)
	}
}
