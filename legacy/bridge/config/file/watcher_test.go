package file

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// FileWatcher Tests
//
// Validates FileWatcher lifecycle and configuration.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                      FileWatcher State Machine                          │
// ├─────────────────────────────────────────────────────────────────────────┤
// │     ○ IDLE ──Start()──▶ RUNNING ──file event──▶ [debounce] ──▶ emit     │
// │         ▲                   │                                            │
// │         │                   │                                            │
// │         └────── Stop() ─────┘                                            │
// └─────────────────────────────────────────────────────────────────────────┘
//
// Note: File system events are inherently nondeterministic in timing.
// These tests focus on the watcher's lifecycle and configuration, not
// precise timing of file events.
// ═══════════════════════════════════════════════════════════════════════════

// createTestFile creates a temporary file for watcher testing.
func createTestFile(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "watched.yaml")
	if err := os.WriteFile(filePath, []byte("test: data"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	return filePath
}

// ═══════════════════════════════════════════════════════════════════════════
// Creation Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewFileWatcher validates watcher creation with valid path.
func TestNewFileWatcher(t *testing.T) {
	filePath := createTestFile(t)

	watcher, err := NewFileWatcher(filePath)
	if err != nil {
		t.Fatalf("NewFileWatcher() error = %v", err)
	}
	defer watcher.Stop()

	if watcher.path != filePath {
		t.Errorf("watcher.path = %q, want %q", watcher.path, filePath)
	}
	if watcher.debounce != 100*time.Millisecond {
		t.Errorf("watcher.debounce = %v, want %v", watcher.debounce, 100*time.Millisecond)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Configuration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileWatcher_SetDebounce validates debounce configuration.
func TestFileWatcher_SetDebounce(t *testing.T) {
	filePath := createTestFile(t)

	watcher, err := NewFileWatcher(filePath)
	if err != nil {
		t.Fatalf("NewFileWatcher() error = %v", err)
	}
	defer watcher.Stop()

	newDebounce := 500 * time.Millisecond
	watcher.SetDebounce(newDebounce)

	if watcher.debounce != newDebounce {
		t.Errorf("debounce = %v, want %v", watcher.debounce, newDebounce)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Lifecycle Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestFileWatcher_Stop validates stopping a non-running watcher.
func TestFileWatcher_Stop_NotRunning(t *testing.T) {
	filePath := createTestFile(t)

	watcher, err := NewFileWatcher(filePath)
	if err != nil {
		t.Fatalf("NewFileWatcher() error = %v", err)
	}

	// Stop before starting should be safe (no-op)
	watcher.Stop()

	// Should still be able to stop again
	watcher.Stop()
}

// TestFileWatcher_Stop_Running validates stopping a running watcher.
func TestFileWatcher_Stop_Running(t *testing.T) {
	filePath := createTestFile(t)

	watcher, err := NewFileWatcher(filePath)
	if err != nil {
		t.Fatalf("NewFileWatcher() error = %v", err)
	}

	// Start the watcher with a cancellable context
	ctx := t.Context()
	_, err = watcher.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Stop the watcher
	watcher.Stop()

	// Verify it's stopped
	if watcher.running {
		t.Error("watcher.running = true after Stop(), want false")
	}
}

// TestFileWatcher_MultipleStops validates multiple stops are safe.
func TestFileWatcher_MultipleStops(t *testing.T) {
	filePath := createTestFile(t)

	watcher, err := NewFileWatcher(filePath)
	if err != nil {
		t.Fatalf("NewFileWatcher() error = %v", err)
	}

	ctx := t.Context()
	_, err = watcher.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Multiple stops should not panic
	watcher.Stop()
	watcher.Stop()
	watcher.Stop()
}
