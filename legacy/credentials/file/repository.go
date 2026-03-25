// Package file provides a file-based credentials repository.
// Credentials are stored as JSON files in a directory hierarchy.
//
// URI format: file://namespace/path/to/creds
// Maps to: basePath/namespace/path/to/creds.json
//
// Example:
//
//	repo := file.New("/var/credentials", file.WithNamespace("tenantA/app1"))
//	creds, err := repo.GetCredentials("file://tenantA/app1/mqtt-creds")
package file

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

const (
	// Scheme is the URI scheme for file-based credentials.
	Scheme = "file"
	// FileExtension is the extension used for credentials files.
	FileExtension = ".json"
)

// storedCredentials wraps credentials with metadata for file storage.
type storedCredentials struct {
	Credentials *types.Credentials `json:"credentials"`
	Version     int64              `json:"version"`
	CreatedAt   time.Time          `json:"createdAt"`
	UpdatedAt   time.Time          `json:"updatedAt"`
}

// Repository implements both CredentialsRepository and CredentialsAdminRepository
// using the local filesystem.
type Repository struct {
	basePath  string
	namespace string
	mu        sync.RWMutex
}

// Option configures a Repository.
type Option func(*Repository)

// WithNamespace sets the namespace for the repository.
func WithNamespace(namespace string) Option {
	return func(r *Repository) {
		r.namespace = namespace
	}
}

// New creates a new file-based credentials repository.
// basePath is the root directory for credentials files.
func New(basePath string, opts ...Option) (*Repository, error) {
	if basePath == "" {
		return nil, fmt.Errorf("basePath is required")
	}

	// Ensure base path exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}

	r := &Repository{
		basePath: basePath,
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// GetScheme returns the URI scheme for this repository.
func (r *Repository) GetScheme() string {
	return Scheme
}

// GetNamespace returns the namespace filter for this repository.
func (r *Repository) GetNamespace() string {
	return r.namespace
}

// GetCredentials retrieves credentials from the file system.
// URI format: file://namespace/path/to/creds
func (r *Repository) GetCredentials(serverURI string) (*types.Credentials, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	filePath, err := r.uriToPath(serverURI)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, types.ErrNotFound
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	return stored.Credentials, nil
}

// CreateCredentials creates new credentials at the given serverURI.
func (r *Repository) CreateCredentials(ctx context.Context, serverURI string, creds *types.Credentials) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(serverURI)
	if err != nil {
		return err
	}

	// Check if file already exists
	if _, err := os.Stat(filePath); err == nil {
		return types.ErrAlreadyExists
	}

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	now := time.Now()
	stored := storedCredentials{
		Credentials: creds,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	return r.writeCredentials(filePath, &stored)
}

// UpdateCredentials updates existing credentials at the given serverURI.
func (r *Repository) UpdateCredentials(ctx context.Context, serverURI string, creds *types.Credentials, version int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(serverURI)
	if err != nil {
		return err
	}

	// Read existing credentials
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return types.ErrNotFound
		}
		return fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("failed to parse credentials file: %w", err)
	}

	// Check version for optimistic locking
	if version > 0 && stored.Version != version {
		return types.ErrVersionMismatch
	}

	// Update
	stored.Credentials = creds
	stored.Version++
	stored.UpdatedAt = time.Now()

	return r.writeCredentials(filePath, &stored)
}

// DeleteCredentials deletes credentials at the given serverURI.
func (r *Repository) DeleteCredentials(ctx context.Context, serverURI string, version int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(serverURI)
	if err != nil {
		return err
	}

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return types.ErrNotFound
	}

	// If version checking is requested, verify version
	if version > 0 {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read credentials file: %w", err)
		}

		var stored storedCredentials
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("failed to parse credentials file: %w", err)
		}

		if stored.Version != version {
			return types.ErrVersionMismatch
		}
	}

	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete credentials file: %w", err)
	}

	return nil
}

// ListCredentials lists all credential URIs in this repository.
func (r *Repository) ListCredentials(ctx context.Context, prefix string) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var uris []string

	searchPath := r.basePath
	if prefix != "" {
		searchPath = filepath.Join(r.basePath, prefix)
	}

	err := filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, FileExtension) {
			return nil
		}

		// Convert file path back to URI
		uri, err := r.pathToURI(path)
		if err != nil {
			return nil // Skip invalid files
		}

		uris = append(uris, uri)
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to list credentials: %w", err)
	}

	return uris, nil
}

// uriToPath converts a serverURI to a file path.
func (r *Repository) uriToPath(serverURI string) (string, error) {
	u, err := url.Parse(serverURI)
	if err != nil {
		return "", fmt.Errorf("invalid URI: %w", err)
	}

	if u.Scheme != Scheme {
		return "", fmt.Errorf("expected scheme %s, got %s", Scheme, u.Scheme)
	}

	// Combine host and path
	path := u.Host
	if u.Path != "" && u.Path != "/" {
		path = path + u.Path
	}

	// Clean up
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	// Add file extension
	if !strings.HasSuffix(path, FileExtension) {
		path = path + FileExtension
	}

	return filepath.Join(r.basePath, path), nil
}

// pathToURI converts a file path back to a serverURI.
func (r *Repository) pathToURI(filePath string) (string, error) {
	// Remove base path
	relPath, err := filepath.Rel(r.basePath, filePath)
	if err != nil {
		return "", err
	}

	// Remove file extension
	relPath = strings.TrimSuffix(relPath, FileExtension)

	// Convert to URI
	return fmt.Sprintf("%s://%s", Scheme, relPath), nil
}

// writeCredentials writes credentials to a file.
func (r *Repository) writeCredentials(filePath string, stored *storedCredentials) error {
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write credentials file: %w", err)
	}

	return nil
}

// GetMetadata retrieves metadata for credentials at the given URI.
func (r *Repository) GetMetadata(serverURI string) (*types.CredentialsMetadata, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	filePath, err := r.uriToPath(serverURI)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, types.ErrNotFound
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	return &types.CredentialsMetadata{
		URI:       serverURI,
		Version:   stored.Version,
		CreatedAt: stored.CreatedAt,
		UpdatedAt: stored.UpdatedAt,
		Types:     stored.Credentials.Type,
	}, nil
}

// Ensure Repository implements both interfaces
var _ types.CredentialsRepository = (*Repository)(nil)
var _ types.CredentialsAdminRepository = (*Repository)(nil)
