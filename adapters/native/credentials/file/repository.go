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

	// 0700: the directory tree holds secret material — a group/world
	// listable directory leaks the set of credential names even when
	// the files themselves are 0600.
	if err := os.MkdirAll(basePath, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}
	if err := tightenDirPermissions(basePath); err != nil {
		return nil, err
	}

	r := &Repository{basePath: basePath, clk: clock.System}
	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// tightenDirPermissions chmods a pre-existing credentials directory to
// 0700 when its current mode grants any group/other access. MkdirAll
// does not touch the mode of directories that already exist, so
// without this a repository pointed at an old 0755 tree would keep
// leaking credential names forever.
func tightenDirPermissions(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat base path: %w", err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("failed to restrict base path permissions to 0700: %w", err)
	}
	return nil
}

func (r *Repository) Scheme() string    { return Scheme }
func (r *Repository) Namespace() string { return r.namespace }

func (r *Repository) Get(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		return nil, shared.ErrUnavailable.WithMessage("failed to read credentials file").Wrap(err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage("failed to parse credentials file").Wrap(err)
	}

	return stored.Credentials.toDomain(), nil
}

func (r *Repository) Create(ctx context.Context, uri string, creds *connectivity.CredentialSet) error {
	if err := ctx.Err(); err != nil {
		return err
	}

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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return shared.ErrUnavailable.WithMessage("failed to create directory").Wrap(err)
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
	if err := ctx.Err(); err != nil {
		return err
	}

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
		return shared.ErrUnavailable.WithMessage("failed to read credentials file").Wrap(err)
	}

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return shared.ErrInvalidPayload.WithMessage("failed to parse credentials file").Wrap(err)
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
	if err := ctx.Err(); err != nil {
		return err
	}

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
			return shared.ErrUnavailable.WithMessage("failed to read credentials file").Wrap(err)
		}

		var stored storedCredentials
		if err := json.Unmarshal(data, &stored); err != nil {
			return shared.ErrInvalidPayload.WithMessage("failed to parse credentials file").Wrap(err)
		}

		if stored.Version != version {
			return shared.ErrVersionMismatch
		}
	}

	if err := os.Remove(filePath); err != nil {
		return shared.ErrUnavailable.WithMessage("failed to delete credentials file").Wrap(err)
	}

	return nil
}

func (r *Repository) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		// Abort a large walk promptly if the caller cancelled (N5): the
		// entry-time ctx check above only covers the first entry.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
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

// ensureUnderBasePath rejects paths that escape the credentials base
// directory. The lexical prefix check catches ../ traversal in the URI;
// the symlink resolution catches the case where a path component under
// the base is a symlink pointing outside it (a lexical check alone is
// blind to that). Both sides are resolved so a symlinked base directory
// (e.g. macOS /tmp -> /private/tmp) compares consistently.
func (r *Repository) ensureUnderBasePath(resolved string) error {
	cleanBase := filepath.Clean(r.basePath)
	base := cleanBase + string(filepath.Separator)
	if !strings.HasPrefix(resolved, base) && resolved != cleanBase {
		return fmt.Errorf("path escapes base directory")
	}

	realBase, err := filepath.EvalSymlinks(cleanBase)
	if err != nil {
		return fmt.Errorf("file credentials: resolve base path: %w", err)
	}
	realPath, err := resolveExistingPrefix(resolved)
	if err != nil {
		return fmt.Errorf("file credentials: resolve path: %w", err)
	}
	if realPath != realBase && !strings.HasPrefix(realPath, realBase+string(filepath.Separator)) {
		return fmt.Errorf("path escapes base directory")
	}
	return nil
}

// resolveExistingPrefix resolves symlinks in path even when its tail
// components do not exist yet (Create writes new files): it walks up to
// the nearest existing ancestor, resolves that through EvalSymlinks,
// and rejoins the non-existing remainder lexically.
func resolveExistingPrefix(path string) (string, error) {
	existing := path
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("file credentials: lstat %s: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// Reached the filesystem root without finding an existing
			// component; nothing to resolve.
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("file credentials: resolve symlinks in %s: %w", existing, err)
	}
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

func (r *Repository) writeCredentials(filePath string, stored *storedCredentials) error {
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return shared.ErrInvalidPayload.WithMessage("failed to marshal credentials").Wrap(err)
	}

	return atomicWriteFile(filePath, data, 0o600)
}

// atomicWriteFile writes data to a temp file in the destination directory,
// fsyncs it, atomically renames it over path, then fsyncs the directory so the
// rename is durable (I3). A crash mid-write or a concurrent external reader can
// therefore only ever observe the complete old file or the complete new file —
// never a truncated or partially written secret. The temp file is 0600 from
// creation and removed on every error path.
//
// ponytail: POSIX atomic-rename + fsync ceiling. Sufficient for local/dev and
// immutable-mount deployments this store targets; a multi-writer or
// multi-process rotating secret store would move to a real secrets manager
// (SSM/Secrets Manager adapters) rather than shared files. Directory fsync is a
// no-op-or-error on some non-POSIX filesystems; the target platforms are
// Linux/macOS.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".credtmp-*")
	if err != nil {
		return shared.ErrUnavailable.WithMessage("failed to create temp credentials file").Wrap(err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup. After a successful rename tmpName no longer exists,
	// so this Remove fails harmlessly; on any error path it deletes the partial
	// temp file so no secret residue is left behind.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return shared.ErrUnavailable.WithMessage("failed to chmod temp credentials file").Wrap(err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return shared.ErrUnavailable.WithMessage("failed to write temp credentials file").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return shared.ErrUnavailable.WithMessage("failed to fsync temp credentials file").Wrap(err)
	}
	if err := tmp.Close(); err != nil {
		return shared.ErrUnavailable.WithMessage("failed to close temp credentials file").Wrap(err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return shared.ErrUnavailable.WithMessage("failed to rename credentials file into place").Wrap(err)
	}

	return fsyncDir(dir)
}

// fsyncDir flushes a directory entry so a preceding rename survives a crash.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return shared.ErrUnavailable.WithMessage("failed to open credentials dir for fsync").Wrap(err)
	}
	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return shared.ErrUnavailable.WithMessage("failed to fsync credentials dir").Wrap(err)
	}
	return nil
}
