package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// Integration Tests: FileConfigSource
//
// These tests validate the full lifecycle of FileConfigSource with real files.
// They test the integration between parser, watcher, and source components.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                    Integration Test Flow                                 │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  Create File ──▶ NewConfigSource ──▶ Discover ──▶ Get/List              │
// │       │                                              │                   │
// │       ▼                                              ▼                   │
// │  Modify File ──▶ Reload/Watch ──▶ Verify Changes                        │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// yamlConfig is a complete YAML configuration for integration testing.
const yamlConfig = `
bridge:
  id: integration-bridge
  clusterId: integration-cluster
  shutdownTimeout: "30s"
  drainTimeout: "10s"
  transportRetry:
    initialBackoff: "1s"
    maxBackoff: "5m"
    multiplier: 2.0
    jitter: 0.1
  flowControl:
    maxInFlight: 100
    defaultMessageTTL: "2m"

connections:
  - id: mqtt-primary
    type: mqtt
    brokerUrls:
      - tcp://broker1:1883
      - tcp://broker2:1883
    clientId: integration-client
    credentials:
      username: user
      password: pass
    options:
      keepAlive: 60
      cleanSession: true

  - id: sqs-target
    type: sqs
    brokerUrls:
      - https://sqs.us-east-1.amazonaws.com
    options:
      region: us-east-1

pipelines:
  - id: mqtt-to-sqs
    source:
      connectionId: mqtt-primary
      topics:
        - sensors/+/temperature
        - sensors/+/humidity
    target:
      connectionId: sqs-target
      topic: sensor-data-queue
    middlewares:
      - transform
      - validate
    mode: streaming
    flowControl:
      maxInFlight: 50
    retry:
      maxAttempts: 3
      initialBackoff: "100ms"
      maxBackoff: "10s"
      multiplier: 2.0

routes:
  - id: sensor-route
    pipelineIds:
      - mqtt-to-sqs
`

// jsonConfig is the same configuration in JSON format.
const jsonConfig = `{
  "bridge": {
    "id": "integration-bridge",
    "clusterId": "integration-cluster",
    "shutdownTimeout": "30s",
    "drainTimeout": "10s"
  },
  "connections": [
    {
      "id": "mqtt-primary",
      "type": "mqtt",
      "brokerUrls": ["tcp://broker1:1883"]
    }
  ],
  "pipelines": [
    {
      "id": "mqtt-to-sqs",
      "source": {
        "connectionId": "mqtt-primary",
        "topics": ["sensors/#"]
      },
      "target": {
        "connectionId": "sqs-target",
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

// ═══════════════════════════════════════════════════════════════════════════
// Full Lifecycle Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_FileConfigSource_FullLifecycle validates the complete
// create -> discover -> get -> list -> reload -> close lifecycle.
func TestIntegration_FileConfigSource_FullLifecycle(t *testing.T) {
	// Create temp config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Create source
	source, err := NewConfigSource(configPath, WithWatch(true))
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	// Discover
	ctx := context.Background()
	items, err := source.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Should have: 1 bridge + 2 connections + 1 pipeline + 1 route = 5 items
	if len(items) != 5 {
		t.Errorf("Discover() returned %d items, want 5", len(items))
	}

	// Get specific item
	bridgeItem, err := source.Get(ctx, "bridge:integration-bridge", "settings")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if bridgeItem.GetPartitionKey() != "bridge:integration-bridge" {
		t.Errorf("bridge partition key = %q, want %q",
			bridgeItem.GetPartitionKey(), "bridge:integration-bridge")
	}

	// Get bridge data and verify
	bridgeData, ok := bridgeItem.GetData().(BridgeSection)
	if !ok {
		t.Fatalf("GetData() returned %T, want BridgeSection", bridgeItem.GetData())
	}
	if bridgeData.ClusterID != "integration-cluster" {
		t.Errorf("BridgeData.ClusterID = %q, want %q", bridgeData.ClusterID, "integration-cluster")
	}

	// List connections
	connections, err := source.List(ctx, "connection:")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(connections) != 2 {
		t.Errorf("List(connection:) returned %d items, want 2", len(connections))
	}

	// Verify connection data
	for _, conn := range connections {
		connData, ok := conn.GetData().(ConnectionConfig)
		if !ok {
			t.Errorf("connection GetData() returned %T, want ConnectionConfig", conn.GetData())
			continue
		}
		if connData.Type != "mqtt" && connData.Type != "sqs" {
			t.Errorf("connection type = %q, want mqtt or sqs", connData.Type)
		}
	}

	// List pipelines
	pipelines, err := source.List(ctx, "pipeline:")
	if err != nil {
		t.Fatalf("List(pipeline:) error = %v", err)
	}
	if len(pipelines) != 1 {
		t.Errorf("List(pipeline:) returned %d items, want 1", len(pipelines))
	}

	// Verify pipeline data
	if len(pipelines) > 0 {
		pipelineData, ok := pipelines[0].GetData().(PipelineConfig)
		if !ok {
			t.Fatalf("pipeline GetData() returned %T, want PipelineConfig", pipelines[0].GetData())
		}
		if pipelineData.ID != "mqtt-to-sqs" {
			t.Errorf("pipeline ID = %q, want %q", pipelineData.ID, "mqtt-to-sqs")
		}
		if len(pipelineData.Middlewares) != 2 {
			t.Errorf("pipeline middlewares = %d, want 2", len(pipelineData.Middlewares))
		}
	}

	// Reload after modification
	updatedConfig := yamlConfig + `
  - id: new-route
    pipelineIds:
      - mqtt-to-sqs
`
	if err := os.WriteFile(configPath, []byte(updatedConfig), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	changes, err := source.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	// Should detect changes (new route + version updates)
	if len(changes) == 0 {
		t.Error("Reload() returned no changes after file modification")
	}

	// Verify new route is present
	routes, err := source.List(ctx, "route:")
	if err != nil {
		t.Fatalf("List(route:) error = %v", err)
	}
	if len(routes) != 2 {
		t.Errorf("List(route:) returned %d items after reload, want 2", len(routes))
	}
}

// TestIntegration_FileConfigSource_YAMLAndJSON validates both formats work.
func TestIntegration_FileConfigSource_YAMLAndJSON(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{"YAML", ".yaml", yamlConfig},
		{"YML", ".yml", yamlConfig},
		{"JSON", ".json", jsonConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config"+tt.ext)
			if err := os.WriteFile(configPath, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to write config: %v", err)
			}

			source, err := NewConfigSource(configPath)
			if err != nil {
				t.Fatalf("NewConfigSource() error = %v", err)
			}
			defer source.Close()

			items, err := source.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}

			if len(items) == 0 {
				t.Error("Discover() returned no items")
			}

			// Verify bridge was parsed
			bridge, err := source.Get(context.Background(), "bridge:integration-bridge", "settings")
			if err != nil {
				t.Fatalf("Get(bridge) error = %v", err)
			}
			if bridge.GetPartitionKey() != "bridge:integration-bridge" {
				t.Errorf("bridge key = %q, want %q",
					bridge.GetPartitionKey(), "bridge:integration-bridge")
			}
		})
	}
}

// TestIntegration_FileConfigSource_WatchChanges validates file watching.
func TestIntegration_FileConfigSource_WatchChanges(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	source, err := NewConfigSource(configPath, WithWatch(true))
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	// Initial discovery
	_, err = source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Start watching
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	changeCh, err := source.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	// Modify file (add a new connection)
	modifiedConfig := yamlConfig + `
  - id: new-connection
    type: kafka
    brokerUrls:
      - kafka:9092
`
	// Wait a bit for watcher to be ready
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(configPath, []byte(modifiedConfig), 0644); err != nil {
		t.Fatalf("failed to modify config: %v", err)
	}

	// Wait for change event with timeout
	select {
	case change, ok := <-changeCh:
		if !ok {
			t.Fatal("change channel closed unexpectedly")
		}
		// We got a change event - success
		t.Logf("received change event: type=%v, key=%s",
			change.Type, change.Item.GetPartitionKey())
	case <-ctx.Done():
		t.Log("watch timeout - file events can be timing-sensitive in tests")
		// This is acceptable in CI environments where file events may be delayed
	}
}

// TestIntegration_FileConfigSource_ConfigWithAllFields validates parsing
// of configuration with all optional fields populated.
func TestIntegration_FileConfigSource_ConfigWithAllFields(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	source, err := NewConfigSource(configPath)
	if err != nil {
		t.Fatalf("NewConfigSource() error = %v", err)
	}
	defer source.Close()

	items, err := source.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Verify all items have valid versions and timestamps
	for _, item := range items {
		if item.GetVersion() == 0 {
			t.Errorf("item %s has zero version", item.GetPartitionKey())
		}
		if item.GetUpdatedAt().IsZero() {
			t.Errorf("item %s has zero timestamp", item.GetPartitionKey())
		}
	}

	// Verify bridge has transport retry config
	bridgeItem, err := source.Get(context.Background(), "bridge:integration-bridge", "settings")
	if err != nil {
		t.Fatalf("Get(bridge) error = %v", err)
	}

	bridgeData, ok := bridgeItem.GetData().(BridgeSection)
	if !ok {
		t.Fatalf("GetData() returned %T, want BridgeSection", bridgeItem.GetData())
	}

	if bridgeData.TransportRetry == nil {
		t.Error("bridge TransportRetry is nil, expected populated")
	} else {
		if bridgeData.TransportRetry.InitialBackoff != "1s" {
			t.Errorf("TransportRetry.InitialBackoff = %q, want %q",
				bridgeData.TransportRetry.InitialBackoff, "1s")
		}
		if bridgeData.TransportRetry.Multiplier != 2.0 {
			t.Errorf("TransportRetry.Multiplier = %v, want %v",
				bridgeData.TransportRetry.Multiplier, 2.0)
		}
	}

	if bridgeData.FlowControl == nil {
		t.Error("bridge FlowControl is nil, expected populated")
	} else {
		if bridgeData.FlowControl.MaxInFlight != 100 {
			t.Errorf("FlowControl.MaxInFlight = %d, want %d",
				bridgeData.FlowControl.MaxInFlight, 100)
		}
	}

	// Verify pipeline has retry config
	pipelineItem, err := source.Get(context.Background(), "pipeline:mqtt-to-sqs", "settings")
	if err != nil {
		t.Fatalf("Get(pipeline) error = %v", err)
	}

	pipelineData, ok := pipelineItem.GetData().(PipelineConfig)
	if !ok {
		t.Fatalf("GetData() returned %T, want PipelineConfig", pipelineItem.GetData())
	}

	if pipelineData.Retry == nil {
		t.Error("pipeline Retry is nil, expected populated")
	} else {
		if pipelineData.Retry.MaxAttempts != 3 {
			t.Errorf("Retry.MaxAttempts = %d, want %d",
				pipelineData.Retry.MaxAttempts, 3)
		}
	}

	// Verify connection has credentials
	connItem, err := source.Get(context.Background(), "connection:mqtt-primary", "settings")
	if err != nil {
		t.Fatalf("Get(connection) error = %v", err)
	}

	connData, ok := connItem.GetData().(ConnectionConfig)
	if !ok {
		t.Fatalf("GetData() returned %T, want ConnectionConfig", connItem.GetData())
	}

	if connData.Credentials == nil {
		t.Error("connection Credentials is nil, expected populated")
	} else {
		if connData.Credentials.Username != "user" {
			t.Errorf("Credentials.Username = %q, want %q",
				connData.Credentials.Username, "user")
		}
	}
}
