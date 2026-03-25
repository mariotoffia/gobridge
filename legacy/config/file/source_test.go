package file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// File ConfigSource Unit Tests
//
// Tests for the file-based ConfigSource implementation covering:
// - Discovery of configuration files
// - Reading individual config items
// - Listing items by partition key
// - Writing and deleting config items
// - File watching (polling mode)
// ═══════════════════════════════════════════════════════════════════════════

// TestSource_Discover validates that Discover loads all JSON config files.
//
// Scenario:
// ─────────────────────────────────────────────────────────────────────────
//
//	Create temp dir → Write 2 JSON files → Discover → Verify 2 items loaded
//
// ─────────────────────────────────────────────────────────────────────────
func TestSource_Discover(t *testing.T) {
	// Setup
	tmpDir := t.TempDir()

	item1 := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:test-1",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		Data:         map[string]any{"name": "test-pipeline-1"},
		UpdatedAt:    time.Now(),
	}

	item2 := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:test-2",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		Data:         map[string]any{"name": "test-pipeline-2"},
		UpdatedAt:    time.Now(),
	}

	writeConfigFile(t, tmpDir, "item1.json", item1)
	writeConfigFile(t, tmpDir, "item2.json", item2)

	// Create source
	source, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	// Discover
	ctx := context.Background()
	items, err := source.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// Assertions
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

// TestSource_Get validates retrieval of a specific config item.
func TestSource_Get(t *testing.T) {
	tmpDir := t.TempDir()

	item := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:test",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		Data:         map[string]any{"name": "test-pipeline"},
		UpdatedAt:    time.Now(),
	}

	writeConfigFile(t, tmpDir, "test.json", item)

	source, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	ctx := context.Background()
	_, err = source.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// Get existing item
	got, err := source.Get(ctx, "bridge:main", "pipeline:test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if got.GetPartitionKey() != item.PartitionKey {
		t.Errorf("partition key mismatch: got %s, want %s", got.GetPartitionKey(), item.PartitionKey)
	}

	// Get non-existent item
	_, err = source.Get(ctx, "bridge:main", "pipeline:nonexistent")
	if err != types.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestSource_List validates listing items by partition key.
func TestSource_List(t *testing.T) {
	tmpDir := t.TempDir()

	items := []ConfigItem{
		{PartitionKey: "bridge:main", SortKey: "pipeline:a", Type: types.ConfigItemTypePipeline, Version: 1, UpdatedAt: time.Now()},
		{PartitionKey: "bridge:main", SortKey: "pipeline:b", Type: types.ConfigItemTypePipeline, Version: 1, UpdatedAt: time.Now()},
		{PartitionKey: "bridge:other", SortKey: "pipeline:c", Type: types.ConfigItemTypePipeline, Version: 1, UpdatedAt: time.Now()},
	}

	for i, item := range items {
		writeConfigFile(t, tmpDir, filepath.Base(item.SortKey)+".json", item)
		_ = i
	}

	source, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	ctx := context.Background()
	_, err = source.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// List items for bridge:main
	mainItems, err := source.List(ctx, "bridge:main")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(mainItems) != 2 {
		t.Errorf("expected 2 items for bridge:main, got %d", len(mainItems))
	}

	// List items for bridge:other
	otherItems, err := source.List(ctx, "bridge:other")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(otherItems) != 1 {
		t.Errorf("expected 1 item for bridge:other, got %d", len(otherItems))
	}
}

// TestSource_Write validates writing a new config item.
func TestSource_Write(t *testing.T) {
	tmpDir := t.TempDir()

	source, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	ctx := context.Background()

	// Create a new item
	item := &ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:new",
		Type:         types.ConfigItemTypePipeline,
		Version:      0, // New item
		Data:         map[string]any{"name": "new-pipeline"},
		UpdatedAt:    time.Now(),
	}

	err = source.Write(ctx, item)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify item can be retrieved
	got, err := source.Get(ctx, "bridge:main", "pipeline:new")
	if err != nil {
		t.Fatalf("Get after Write failed: %v", err)
	}

	if got.GetVersion() != 1 {
		t.Errorf("expected version 1 after write, got %d", got.GetVersion())
	}

	// Verify file was created
	files, err := filepath.Glob(filepath.Join(tmpDir, "*.json"))
	if err != nil {
		t.Fatalf("failed to glob files: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d", len(files))
	}
}

// TestSource_Delete validates deleting a config item.
func TestSource_Delete(t *testing.T) {
	tmpDir := t.TempDir()

	item := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:delete-me",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		Data:         map[string]any{"name": "to-delete"},
		UpdatedAt:    time.Now(),
	}

	writeConfigFile(t, tmpDir, "delete.json", item)

	source, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	ctx := context.Background()
	_, err = source.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// Delete the item
	err = source.Delete(ctx, "bridge:main", "pipeline:delete-me", 1)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify item is gone
	_, err = source.Get(ctx, "bridge:main", "pipeline:delete-me")
	if err != types.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}

	// Verify file was deleted
	files, err := filepath.Glob(filepath.Join(tmpDir, "*.json"))
	if err != nil {
		t.Fatalf("failed to glob files: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("expected 0 files after delete, got %d", len(files))
	}
}

// TestSource_WatchPolling validates polling-based file watching.
func TestSource_WatchPolling(t *testing.T) {
	tmpDir := t.TempDir()

	source, err := NewSource(tmpDir, WithWatchInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer source.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start watching
	changeCh, err := source.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	// Give watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Add a new file
	item := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:watched",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		Data:         map[string]any{"name": "watched"},
		UpdatedAt:    time.Now(),
	}
	writeConfigFile(t, tmpDir, "watched.json", item)

	// Wait for change notification
	select {
	case change := <-changeCh:
		if change.Type != types.ConfigChangeAdd {
			t.Errorf("expected ConfigChangeAdd, got %v", change.Type)
		}
	case <-ctx.Done():
		t.Error("timed out waiting for change notification")
	}
}

// TestSource_RecursiveDiscovery validates recursive directory scanning.
func TestSource_RecursiveDiscovery(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	item1 := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:root",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		UpdatedAt:    time.Now(),
	}

	item2 := ConfigItem{
		PartitionKey: "bridge:main",
		SortKey:      "pipeline:nested",
		Type:         types.ConfigItemTypePipeline,
		Version:      1,
		UpdatedAt:    time.Now(),
	}

	writeConfigFile(t, tmpDir, "root.json", item1)
	writeConfigFile(t, subDir, "nested.json", item2)

	// Non-recursive should only find 1
	sourceNonRecursive, err := NewSource(tmpDir)
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer sourceNonRecursive.Close()

	ctx := context.Background()
	items, err := sourceNonRecursive.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(items) != 1 {
		t.Errorf("non-recursive: expected 1 item, got %d", len(items))
	}

	// Recursive should find 2
	sourceRecursive, err := NewSource(tmpDir, WithRecursive(true))
	if err != nil {
		t.Fatalf("failed to create source: %v", err)
	}
	defer sourceRecursive.Close()

	items, err = sourceRecursive.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(items) != 2 {
		t.Errorf("recursive: expected 2 items, got %d", len(items))
	}
}

// writeConfigFile is a test helper that writes a ConfigItem to a JSON file.
func writeConfigFile(t *testing.T, dir, name string, item ConfigItem) {
	t.Helper()

	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal item: %v", err)
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}
