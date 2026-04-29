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

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.CredentialRepository = (*Repository)(nil)
	_ ports.CredentialAdmin      = (*Repository)(nil)
)

const (
	Scheme        = "file"
	FileExtension = ".json"
)

type storedCredentials struct {
	Credentials *domain.CredentialSet `json:"credentials"`
	Version     int64                 `json:"version"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
}

type Repository struct {
	basePath  string
	namespace string
	mu        sync.RWMutex
}

type Option func(*Repository)

func WithNamespace(namespace string) Option {
	return func(r *Repository) { r.namespace = namespace }
}

func New(basePath string, opts ...Option) (*Repository, error) {
	if basePath == "" {
		return nil, fmt.Errorf("basePath is required")
	}

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}

	r := &Repository{basePath: basePath}
	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

func (r *Repository) Scheme() string    { return Scheme }
func (r *Repository) Namespace() string { return r.namespace }

func (r *Repository) Get(ctx context.Context, uri string) (*domain.CredentialSet, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	return stored.Credentials, nil
}

func (r *Repository) Create(ctx context.Context, uri string, creds *domain.CredentialSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return err
	}

	if _, err := os.Stat(filePath); err == nil {
		return domain.ErrAlreadyExists
	}

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

func (r *Repository) Update(ctx context.Context, uri string, creds *domain.CredentialSet, version int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("failed to parse credentials file: %w", err)
	}

	if version > 0 && stored.Version != version {
		return domain.ErrVersionMismatch
	}

	stored.Credentials = creds
	stored.Version++
	stored.UpdatedAt = time.Now()

	return r.writeCredentials(filePath, &stored)
}

func (r *Repository) Delete(ctx context.Context, uri string, version int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return err
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return domain.ErrNotFound
	}

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
			return domain.ErrVersionMismatch
		}
	}

	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete credentials file: %w", err)
	}

	return nil
}

func (r *Repository) List(ctx context.Context, prefix string) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var uris []string

	searchPath := r.basePath
	if prefix != "" {
		searchPath = filepath.Clean(filepath.Join(r.basePath, prefix))
		if err := r.ensureUnderBasePath(searchPath); err != nil {
			return nil, err
		}
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

		uri, err := r.pathToURI(path)
		if err != nil {
			return nil
		}

		uris = append(uris, uri)
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to list credentials: %w", err)
	}

	return uris, nil
}

func (r *Repository) uriToPath(serverURI string) (string, error) {
	u, err := url.Parse(serverURI)
	if err != nil {
		return "", fmt.Errorf("invalid URI: %w", err)
	}

	if u.Scheme != Scheme {
		return "", fmt.Errorf("expected scheme %s, got %s", Scheme, u.Scheme)
	}

	path := u.Host
	if u.Path != "" && u.Path != "/" {
		path = path + u.Path
	}

	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	if !strings.HasSuffix(path, FileExtension) {
		path = path + FileExtension
	}

	full := filepath.Clean(filepath.Join(r.basePath, path))

	if err := r.ensureUnderBasePath(full); err != nil {
		return "", err
	}

	return full, nil
}

func (r *Repository) pathToURI(filePath string) (string, error) {
	relPath, err := filepath.Rel(r.basePath, filePath)
	if err != nil {
		return "", err
	}

	relPath = strings.TrimSuffix(relPath, FileExtension)
	return fmt.Sprintf("%s://%s", Scheme, relPath), nil
}

func (r *Repository) ensureUnderBasePath(resolved string) error {
	base := filepath.Clean(r.basePath) + string(filepath.Separator)
	if !strings.HasPrefix(resolved, base) && resolved != filepath.Clean(r.basePath) {
		return fmt.Errorf("path escapes base directory")
	}
	return nil
}

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
