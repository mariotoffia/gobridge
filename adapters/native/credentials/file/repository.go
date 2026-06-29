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

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	Credentials *credentialSetDTO `json:"credentials"`
	Version     int64             `json:"version"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// credentialSetDTO mirrors connectivity.CredentialSet on the wire. The domain
// value objects expose only accessors, so persistence goes through these DTOs
// rather than marshalling the domain types directly. JSON keys are kept
// identical to the historical default-marshalled layout for backward
// compatibility with existing credential files.
type credentialSetDTO struct {
	Password *passwordDTO `json:"Password"`
	TLS      *tlsDTO      `json:"TLS"`
}

type passwordDTO struct {
	Username string `json:"Username"`
	Password string `json:"Password"`
}

type tlsDTO struct {
	CertPEM            string   `json:"CertPEM"`
	KeyPEM             string   `json:"KeyPEM"`
	CAPEMs             []string `json:"CAPEMs"`
	InsecureSkipVerify bool     `json:"InsecureSkipVerify"`
}

// toDTO converts a domain CredentialSet to its persistable form, revealing
// the wrapped secrets exactly at this storage boundary.
func toDTO(cs *connectivity.CredentialSet) *credentialSetDTO {
	if cs == nil {
		return nil
	}
	dto := &credentialSetDTO{}
	if cs.Password() != nil {
		dto.Password = &passwordDTO{
			Username: cs.Password().Username(),
			Password: cs.Password().Password().Reveal(),
		}
	}
	if cs.TLS() != nil {
		dto.TLS = &tlsDTO{
			CertPEM:            cs.TLS().CertPEM(),
			KeyPEM:             cs.TLS().KeyPEM().Reveal(),
			CAPEMs:             cs.TLS().CAPEMs(),
			InsecureSkipVerify: cs.TLS().InsecureSkipVerify(),
		}
	}
	return dto
}

// toDomain reconstructs the immutable domain CredentialSet from the DTO.
func (dto *credentialSetDTO) toDomain() *connectivity.CredentialSet {
	if dto == nil {
		return nil
	}
	var pw *connectivity.PasswordCredential
	if dto.Password != nil {
		p := connectivity.NewPasswordCredential(dto.Password.Username, dto.Password.Password)
		pw = &p
	}
	var tls *connectivity.TLSMaterial
	if dto.TLS != nil {
		t := connectivity.NewTLSMaterial(dto.TLS.CertPEM, dto.TLS.KeyPEM, dto.TLS.CAPEMs, dto.TLS.InsecureSkipVerify)
		tls = &t
	}
	return connectivity.NewCredentialSet(pw, tls)
}

type Repository struct {
	basePath  string
	namespace string
	clk       clock.Clock
	mu        sync.RWMutex
}

type Option func(*Repository)

func WithNamespace(namespace string) Option {
	return func(r *Repository) { r.namespace = namespace }
}

// WithClock sets the clock used for persisted credential timestamps.
func WithClock(c clock.Clock) Option {
	return func(r *Repository) {
		if c != nil {
			r.clk = c
		}
	}
}

func New(basePath string, opts ...Option) (*Repository, error) {
	if basePath == "" {
		return nil, fmt.Errorf("basePath is required")
	}

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}

	r := &Repository{basePath: basePath, clk: clock.System}
	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

func (r *Repository) Scheme() string    { return Scheme }
func (r *Repository) Namespace() string { return r.namespace }

func (r *Repository) Get(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, shared.ErrNotFound
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	return stored.Credentials.toDomain(), nil
}

func (r *Repository) Create(ctx context.Context, uri string, creds *connectivity.CredentialSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return err
	}

	if _, err := os.Stat(filePath); err == nil {
		return shared.ErrAlreadyExists
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	now := r.clk.Now()
	stored := storedCredentials{
		Credentials: toDTO(creds),
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	return r.writeCredentials(filePath, &stored)
}

func (r *Repository) Update(ctx context.Context, uri string, creds *connectivity.CredentialSet, version int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filePath, err := r.uriToPath(uri)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return shared.ErrNotFound
		}
		return fmt.Errorf("failed to read credentials file: %w", err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("failed to parse credentials file: %w", err)
	}

	if version > 0 && stored.Version != version {
		return shared.ErrVersionMismatch
	}

	stored.Credentials = toDTO(creds)
	stored.Version++
	stored.UpdatedAt = r.clk.Now()

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
		return shared.ErrNotFound
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
			return shared.ErrVersionMismatch
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
		return "", fmt.Errorf("file credentials: relative path: %w", err)
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
