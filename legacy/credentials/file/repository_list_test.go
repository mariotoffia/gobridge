package file

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// File-Based Credentials Repository List, Metadata & Behavior Tests
//
// Tests for:
// - ListCredentials with prefix filtering
// - Metadata retrieval
// - Version increment behavior
// - Timestamp preservation
// - Concurrency safety
// ═══════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════
// ListCredentials Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestListCredentials_Empty validates listing an empty repository.
func TestListCredentials_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uris, err := repo.ListCredentials(ctx, "")
	if err != nil {
		t.Fatalf("ListCredentials failed: %v", err)
	}

	if len(uris) != 0 {
		t.Errorf("expected empty list, got %v", uris)
	}
}

// TestListCredentials_WithFiles validates listing all credentials.
func TestListCredentials_WithFiles(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	// Create several credentials
	expectedURIs := []string{
		"file://app1/db",
		"file://app1/cache",
		"file://app2/api",
	}

	for _, uri := range expectedURIs {
		creds := testCredentials("user", "pass")
		if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
			t.Fatalf("CreateCredentials failed for %s: %v", uri, err)
		}
	}

	// List all
	uris, err := repo.ListCredentials(ctx, "")
	if err != nil {
		t.Fatalf("ListCredentials failed: %v", err)
	}

	if len(uris) != len(expectedURIs) {
		t.Errorf("expected %d URIs, got %d", len(expectedURIs), len(uris))
	}

	// Verify all expected URIs are present
	uriSet := make(map[string]bool)
	for _, uri := range uris {
		uriSet[uri] = true
	}

	for _, expected := range expectedURIs {
		if !uriSet[expected] {
			t.Errorf("missing expected URI: %s", expected)
		}
	}
}

// TestListCredentials_WithPrefix validates prefix filtering.
func TestListCredentials_WithPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	// Create credentials in different namespaces
	allURIs := []string{
		"file://tenant1/app/db",
		"file://tenant1/app/cache",
		"file://tenant2/app/db",
	}

	for _, uri := range allURIs {
		creds := testCredentials("user", "pass")
		if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
			t.Fatalf("CreateCredentials failed for %s: %v", uri, err)
		}
	}

	// List with prefix
	uris, err := repo.ListCredentials(ctx, "tenant1")
	if err != nil {
		t.Fatalf("ListCredentials failed: %v", err)
	}

	if len(uris) != 2 {
		t.Errorf("expected 2 URIs with prefix tenant1, got %d: %v", len(uris), uris)
	}

	for _, uri := range uris {
		if uri != "file://tenant1/app/db" && uri != "file://tenant1/app/cache" {
			t.Errorf("unexpected URI with prefix: %s", uri)
		}
	}
}

// TestListCredentials_NonexistentPrefix validates listing with no matches.
func TestListCredentials_NonexistentPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	// Create some credentials
	creds := testCredentials("user", "pass")
	if err := repo.CreateCredentials(ctx, "file://exists/creds", creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// List with non-matching prefix
	uris, err := repo.ListCredentials(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListCredentials failed: %v", err)
	}

	if len(uris) != 0 {
		t.Errorf("expected empty list for nonexistent prefix, got %v", uris)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// GetMetadata Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGetMetadata_Success validates metadata retrieval.
func TestGetMetadata_Success(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://meta/test"
	creds := testCredentials("user", "pass")

	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	metadata, err := repo.GetMetadata(uri)
	if err != nil {
		t.Fatalf("GetMetadata failed: %v", err)
	}

	if metadata.URI != uri {
		t.Errorf("expected URI=%s, got %s", uri, metadata.URI)
	}
	if metadata.Version != 1 {
		t.Errorf("expected version=1, got %d", metadata.Version)
	}
	if metadata.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
	if metadata.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set")
	}
	if len(metadata.Types) != 1 || metadata.Types[0] != types.CredentialsTypeUsernamePassword {
		t.Errorf("unexpected Types: %v", metadata.Types)
	}
}

// TestGetMetadata_NotFound validates ErrNotFound for missing credentials.
func TestGetMetadata_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	_, err := repo.GetMetadata("file://nonexistent")

	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Version and Timestamp Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestVersionIncrement validates that version increases on each update.
func TestVersionIncrement(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://versioned"
	creds := testCredentials("user", "pass")

	// Create (version=1)
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Perform multiple updates and verify version increments
	for i := 1; i <= 5; i++ {
		meta, _ := repo.GetMetadata(uri)
		expectedVersion := int64(i)
		if meta.Version != expectedVersion {
			t.Errorf("iteration %d: expected version=%d, got %d", i, expectedVersion, meta.Version)
		}

		// Update without version check
		if err := repo.UpdateCredentials(ctx, uri, creds, 0); err != nil {
			t.Fatalf("UpdateCredentials failed at iteration %d: %v", i, err)
		}
	}

	// Final version should be 6
	meta, _ := repo.GetMetadata(uri)
	if meta.Version != 6 {
		t.Errorf("expected final version=6, got %d", meta.Version)
	}
}

// TestUpdateCredentials_PreservesCreatedAt validates CreatedAt is not modified.
func TestUpdateCredentials_PreservesCreatedAt(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://timestamps"
	creds := testCredentials("user", "pass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Get original metadata
	originalMeta, _ := repo.GetMetadata(uri)
	originalCreatedAt := originalMeta.CreatedAt

	// Wait a bit to ensure different timestamp
	time.Sleep(10 * time.Millisecond)

	// Update
	if err := repo.UpdateCredentials(ctx, uri, creds, 1); err != nil {
		t.Fatalf("UpdateCredentials failed: %v", err)
	}

	// Verify CreatedAt unchanged, UpdatedAt changed
	newMeta, _ := repo.GetMetadata(uri)

	if !newMeta.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt was modified: %v → %v", originalCreatedAt, newMeta.CreatedAt)
	}
	if !newMeta.UpdatedAt.After(originalMeta.UpdatedAt) {
		t.Error("UpdatedAt should be after original")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Concurrency Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestConcurrentReads validates concurrent read operations.
func TestConcurrentReads(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://concurrent/read"
	creds := testCredentials("user", "pass")

	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	var wg sync.WaitGroup
	errChan := make(chan error, 10)

	// Launch 10 concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.GetCredentials(uri)
			if err != nil {
				errChan <- err
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("concurrent read failed: %v", err)
	}
}

// TestConcurrentWrites validates serialized write operations.
func TestConcurrentWrites(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://concurrent/write"
	creds := testCredentials("user", "pass")

	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	// Launch 10 concurrent updaters (without version check)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.UpdateCredentials(ctx, uri, creds, 0)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// All updates should succeed (mutex protects access)
	if successCount != 10 {
		t.Errorf("expected 10 successful updates, got %d", successCount)
	}

	// Final version should reflect all updates
	meta, _ := repo.GetMetadata(uri)
	if meta.Version != 11 { // 1 create + 10 updates
		t.Errorf("expected version=11, got %d", meta.Version)
	}
}
