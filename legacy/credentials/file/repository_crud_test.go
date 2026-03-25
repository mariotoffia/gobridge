package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// File-Based Credentials Repository CRUD Tests
//
// Tests for Create, Read, Update, Delete operations.
// List, metadata, versioning, and concurrency tests are in repository_list_test.go
//
// Error Classification:
//   - ErrNotFound: Credentials file does not exist
//   - ErrAlreadyExists: Credentials file already exists (on create)
//   - ErrVersionMismatch: Optimistic locking failure
// ═══════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════
// CreateCredentials Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestCreateCredentials_Success validates successful credential creation.
//
// Flow: serverURI → uriToPath → os.MkdirAll → writeCredentials
func TestCreateCredentials_Success(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://tenant/app/mqtt"
	creds := testCredentials("user1", "pass1")

	err := repo.CreateCredentials(ctx, uri, creds)
	if err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Verify file exists
	expectedPath := filepath.Join(tmpDir, "tenant", "app", "mqtt.json")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Error("credentials file was not created")
	}

	// Verify content
	data, _ := os.ReadFile(expectedPath)
	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("failed to parse stored credentials: %v", err)
	}

	if stored.Version != 1 {
		t.Errorf("expected version=1, got %d", stored.Version)
	}
	if stored.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

// TestCreateCredentials_AlreadyExists validates ErrAlreadyExists on duplicate.
func TestCreateCredentials_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://existing"
	creds := testCredentials("user", "pass")

	// Create first
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("first CreateCredentials failed: %v", err)
	}

	// Try to create again
	err := repo.CreateCredentials(ctx, uri, creds)
	if !errors.Is(err, types.ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists, got %v", err)
	}
}

// TestCreateCredentials_CreatesNestedDirectories validates deep path creation.
func TestCreateCredentials_CreatesNestedDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://deeply/nested/path/to/credentials"
	creds := testCredentials("user", "pass")

	err := repo.CreateCredentials(ctx, uri, creds)
	if err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	expectedPath := filepath.Join(tmpDir, "deeply", "nested", "path", "to", "credentials.json")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Error("nested directories were not created")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// GetCredentials Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGetCredentials_Success validates successful credential retrieval.
func TestGetCredentials_Success(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://myapp/db"
	creds := testCredentials("dbuser", "dbpass")

	// Create first
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Retrieve
	retrieved, err := repo.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}

	if len(retrieved.Type) != 1 || retrieved.Type[0] != types.CredentialsTypeUsernamePassword {
		t.Errorf("unexpected credential type: %v", retrieved.Type)
	}
}

// TestGetCredentials_NotFound validates ErrNotFound for missing credentials.
func TestGetCredentials_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	_, err := repo.GetCredentials("file://nonexistent")
	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestGetCredentials_InvalidJSON validates error on corrupted file.
func TestGetCredentials_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	// Create invalid JSON file directly
	filePath := filepath.Join(tmpDir, "corrupt.json")
	if err := os.WriteFile(filePath, []byte("not valid json{"), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err := repo.GetCredentials("file://corrupt")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// UpdateCredentials Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestUpdateCredentials_Success validates successful credential update.
//
// Flow:
//  1. Read existing credentials
//  2. Check version (if version > 0)
//  3. Update credentials, increment version
//  4. Write to file
func TestUpdateCredentials_Success(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://app/creds"
	original := testCredentials("user1", "pass1")
	updated := testCredentials("user1", "newpass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, original); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Update with version check
	err := repo.UpdateCredentials(ctx, uri, updated, 1)
	if err != nil {
		t.Fatalf("UpdateCredentials failed: %v", err)
	}

	// Verify version incremented
	metadata, _ := repo.GetMetadata(uri)
	if metadata.Version != 2 {
		t.Errorf("expected version=2, got %d", metadata.Version)
	}
}

// TestUpdateCredentials_NotFound validates ErrNotFound for missing credentials.
func TestUpdateCredentials_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	creds := testCredentials("user", "pass")
	err := repo.UpdateCredentials(ctx, "file://nonexistent", creds, 0)

	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestUpdateCredentials_VersionMismatch validates optimistic locking.
func TestUpdateCredentials_VersionMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://locked"
	creds := testCredentials("user", "pass")

	// Create (version becomes 1)
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Try to update with wrong version
	err := repo.UpdateCredentials(ctx, uri, creds, 99)

	if !errors.Is(err, types.ErrVersionMismatch) {
		t.Errorf("expected ErrVersionMismatch, got %v", err)
	}
}

// TestUpdateCredentials_NoVersionCheck validates update without version check.
func TestUpdateCredentials_NoVersionCheck(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://nocheck"
	creds := testCredentials("user", "pass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Update with version=0 (no check)
	updated := testCredentials("user", "newpass")
	err := repo.UpdateCredentials(ctx, uri, updated, 0)

	if err != nil {
		t.Errorf("expected update without version check to succeed, got %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// DeleteCredentials Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDeleteCredentials_Success validates successful credential deletion.
func TestDeleteCredentials_Success(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://todelete"
	creds := testCredentials("user", "pass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Delete with version check
	err := repo.DeleteCredentials(ctx, uri, 1)
	if err != nil {
		t.Fatalf("DeleteCredentials failed: %v", err)
	}

	// Verify gone
	_, err = repo.GetCredentials(uri)
	if !errors.Is(err, types.ErrNotFound) {
		t.Error("credentials should be deleted")
	}
}

// TestDeleteCredentials_NotFound validates ErrNotFound for missing credentials.
func TestDeleteCredentials_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	err := repo.DeleteCredentials(ctx, "file://nonexistent", 0)

	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestDeleteCredentials_VersionMismatch validates optimistic locking on delete.
func TestDeleteCredentials_VersionMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://lockeddelete"
	creds := testCredentials("user", "pass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Try to delete with wrong version
	err := repo.DeleteCredentials(ctx, uri, 99)

	if !errors.Is(err, types.ErrVersionMismatch) {
		t.Errorf("expected ErrVersionMismatch, got %v", err)
	}
}

// TestDeleteCredentials_NoVersionCheck validates delete without version check.
func TestDeleteCredentials_NoVersionCheck(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)
	ctx := context.Background()

	uri := "file://deletenoverify"
	creds := testCredentials("user", "pass")

	// Create
	if err := repo.CreateCredentials(ctx, uri, creds); err != nil {
		t.Fatalf("CreateCredentials failed: %v", err)
	}

	// Delete with version=0 (no check)
	err := repo.DeleteCredentials(ctx, uri, 0)

	if err != nil {
		t.Errorf("expected delete without version check to succeed, got %v", err)
	}
}
