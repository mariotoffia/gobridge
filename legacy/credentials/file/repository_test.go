package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// File-Based Credentials Repository Unit Tests - Core
//
// Tests for repository construction, configuration, and URI handling.
// CRUD operation tests are in repository_crud_test.go
//
// Data Flow:
//   serverURI ──▶ uriToPath() ──▶ File System (JSON) ──▶ pathToURI()
// ═══════════════════════════════════════════════════════════════════════════

// testCredentials creates test credentials with username/password.
func testCredentials(username, password string) *types.Credentials {
	return &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
		Credentials: []any{
			types.UsernamePasswordCredentials{
				Username: username,
				Password: password,
			},
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNew_Success validates repository creation with valid basePath.
func TestNew_Success(t *testing.T) {
	tmpDir := t.TempDir()

	repo, err := New(tmpDir)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if repo == nil {
		t.Fatal("expected non-nil repository")
	}
	if repo.basePath != tmpDir {
		t.Errorf("expected basePath=%s, got %s", tmpDir, repo.basePath)
	}
}

// TestNew_EmptyBasePath validates rejection of empty basePath.
func TestNew_EmptyBasePath(t *testing.T) {
	_, err := New("")
	if err == nil {
		t.Error("expected error for empty basePath")
	}
}

// TestNew_CreatesDirectory validates that New creates the base directory.
func TestNew_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	newPath := filepath.Join(tmpDir, "creds", "store")

	repo, err := New(newPath)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if repo == nil {
		t.Fatal("expected non-nil repository")
	}

	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected path to be a directory")
	}
}

// TestNew_WithNamespace validates namespace option application.
func TestNew_WithNamespace(t *testing.T) {
	tmpDir := t.TempDir()

	repo, err := New(tmpDir, WithNamespace("tenantA/app1"))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if repo.namespace != "tenantA/app1" {
		t.Errorf("expected namespace=tenantA/app1, got %s", repo.namespace)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Scheme and Namespace Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGetScheme validates the scheme returned.
func TestGetScheme(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	if repo.GetScheme() != "file" {
		t.Errorf("expected scheme=file, got %s", repo.GetScheme())
	}
}

// TestGetNamespace validates namespace retrieval.
func TestGetNamespace(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		namespace string
	}{
		{"empty namespace", ""},
		{"simple namespace", "myapp"},
		{"nested namespace", "tenant/app/env"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option
			if tt.namespace != "" {
				opts = append(opts, WithNamespace(tt.namespace))
			}

			repo, _ := New(tmpDir, opts...)
			if repo.GetNamespace() != tt.namespace {
				t.Errorf("expected namespace=%s, got %s", tt.namespace, repo.GetNamespace())
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// URI to Path Conversion Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestURIToPath_ValidURIs validates URI to file path conversion.
//
// Mapping: file://namespace/path/to/creds → basePath/namespace/path/to/creds.json
func TestURIToPath_ValidURIs(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	tests := []struct {
		name     string
		uri      string
		expected string
	}{
		{
			name:     "simple path",
			uri:      "file://prod/db/password",
			expected: filepath.Join(tmpDir, "prod", "db", "password.json"),
		},
		{
			name:     "nested path",
			uri:      "file://tenant/app/service/creds",
			expected: filepath.Join(tmpDir, "tenant", "app", "service", "creds.json"),
		},
		{
			name:     "single segment",
			uri:      "file://mycreds",
			expected: filepath.Join(tmpDir, "mycreds.json"),
		},
		{
			name:     "with existing extension",
			uri:      "file://path/to/creds.json",
			expected: filepath.Join(tmpDir, "path", "to", "creds.json"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := repo.uriToPath(tt.uri)
			if err != nil {
				t.Fatalf("uriToPath failed: %v", err)
			}
			if path != tt.expected {
				t.Errorf("expected path=%s, got %s", tt.expected, path)
			}
		})
	}
}

// TestURIToPath_InvalidURIs validates rejection of invalid URIs.
func TestURIToPath_InvalidURIs(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	tests := []struct {
		name string
		uri  string
	}{
		{"invalid format", "not-a-uri"},
		{"missing scheme", "://path/to/creds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repo.uriToPath(tt.uri)
			if err == nil {
				t.Error("expected error for invalid URI")
			}
		})
	}
}

// TestURIToPath_WrongScheme validates rejection of non-file schemes.
func TestURIToPath_WrongScheme(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	tests := []struct {
		name string
		uri  string
	}{
		{"pms scheme", "pms://path/to/creds"},
		{"vault scheme", "vault://secret/path"},
		{"https scheme", "https://example.com/creds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repo.uriToPath(tt.uri)
			if err == nil {
				t.Error("expected error for wrong scheme")
			}
		})
	}
}

// TestPathToURI validates file path to URI conversion.
func TestPathToURI(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "simple path",
			path:     filepath.Join(tmpDir, "mycreds.json"),
			expected: "file://mycreds",
		},
		{
			name:     "nested path",
			path:     filepath.Join(tmpDir, "tenant", "app", "creds.json"),
			expected: "file://tenant/app/creds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri, err := repo.pathToURI(tt.path)
			if err != nil {
				t.Fatalf("pathToURI failed: %v", err)
			}
			if uri != tt.expected {
				t.Errorf("expected uri=%s, got %s", tt.expected, uri)
			}
		})
	}
}

// TestURIPathRoundtrip validates that URI → Path → URI produces original URI.
func TestURIPathRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	uris := []string{
		"file://simple",
		"file://tenant/app/creds",
		"file://a/b/c/d/e",
	}

	for _, uri := range uris {
		t.Run(uri, func(t *testing.T) {
			path, err := repo.uriToPath(uri)
			if err != nil {
				t.Fatalf("uriToPath failed: %v", err)
			}

			result, err := repo.pathToURI(path)
			if err != nil {
				t.Fatalf("pathToURI failed: %v", err)
			}

			if result != uri {
				t.Errorf("roundtrip failed: %s → %s → %s", uri, path, result)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Interface Compliance Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestInterfaceCompliance validates that Repository implements required interfaces.
func TestInterfaceCompliance(t *testing.T) {
	tmpDir := t.TempDir()
	repo, _ := New(tmpDir)

	// Verify CredentialsRepository interface
	var _ types.CredentialsRepository = repo

	// Verify CredentialsAdminRepository interface
	var _ types.CredentialsAdminRepository = repo
}
