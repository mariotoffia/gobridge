package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
		// Normalise a whitespace-only cert or key to the empty string. A CA-only
		// document may carry blank-but-non-empty cert/key fields (" ", "\n");
		// ensureUsableCredential judges their presence AFTER trimming, so such a
		// file loads, but storing the fields verbatim would later drive a
		// transport into tls.X509KeyPair(" ", " ") and fail at connect time on
		// material this reader already accepted. A genuine cert+key pair is
		// stored verbatim. Mirrors the sibling SSM parser
		// (adapters/aws/credentials/ssm/parser.go, parseTLSJSON).
		certPEM := dto.TLS.CertPEM
		if strings.TrimSpace(certPEM) == "" {
			certPEM = ""
		}
		keyPEM := dto.TLS.KeyPEM
		if strings.TrimSpace(keyPEM) == "" {
			keyPEM = ""
		}
		t := connectivity.NewTLSMaterial(certPEM, keyPEM, dto.TLS.CAPEMs, dto.TLS.InsecureSkipVerify)
		tls = &t
	}
	return connectivity.NewCredentialSet(pw, tls)
}

// ensureUsableCredential rejects a credential set that carries no usable
// credential material (c12). It runs on every load AND every write so an empty
// set can neither masquerade as valid on read nor silently replace live
// credentials with a no-auth no-op on rotation.
//
// A set is usable when it carries at least one of:
//   - basic-auth material: a non-empty username OR a non-empty password.
//     Username-only is a legitimate broker shape — the paho transport applies
//     set.Password().Username() with whatever password is present, empty
//     included (adapters/mqtt/transport/paho/config_plugin.go) — so gating on
//     the password alone would reject a credential that loaded and connected
//     before this guard existed.
//   - TLS material: a CA bundle (server verification) and/or a COMPLETE
//     cert+key pair (mutual TLS).
//
// A torn TLS half — a lone cert or a lone key — is rejected UNCONDITIONALLY,
// even when basic-auth material is present, so it surfaces here at load/write
// time rather than as a confusing connect-time "requires both" failure in the
// transport (servicebus buildTLSConfig / amqp10 both demand the pair).
// Emptiness is judged after strings.TrimSpace, so whitespace-only material
// counts as absent.
//
// Parity note: the whitespace-trim rule and the CA-or-complete-pair /
// torn-half rejection MATCH the sibling SSM parser
// (adapters/aws/credentials/ssm/parser.go). The basic-auth FIELD gating
// DIVERGES: SSM gates on username only and permits an empty password, whereas
// this repository accepts username-or-password. Unifying the two behind a
// single connectivity.CredentialSet predicate is deferred to a Wave D ADR and
// must NOT be attempted from this closed module.
func ensureUsableCredential(dto *credentialSetDTO) error {
	if dto == nil {
		return errNoUsableCredential()
	}

	// A torn TLS half is always invalid, even alongside usable basic auth.
	if dto.TLS != nil {
		hasCert := strings.TrimSpace(dto.TLS.CertPEM) != ""
		hasKey := strings.TrimSpace(dto.TLS.KeyPEM) != ""
		if hasCert != hasKey {
			return shared.ErrInvalidPayload.WithMessage(
				"incomplete TLS material: certificate and key must both be present")
		}
	}

	if !dto.hasUsableBasicAuth() && !dto.hasUsableTLS() {
		return errNoUsableCredential()
	}
	return nil
}

func errNoUsableCredential() error {
	return shared.ErrInvalidPayload.WithMessage(
		"credential set carries no usable credential (need a username or password, a CA bundle, or a complete cert/key pair)")
}

// hasUsableBasicAuth reports whether the DTO carries usable basic-auth
// material: a non-empty username OR a non-empty password (after trimming).
// Username-only and password-only are both legitimate; only an entry whose
// username AND password are both empty/whitespace contributes nothing.
func (dto *credentialSetDTO) hasUsableBasicAuth() bool {
	if dto.Password == nil {
		return false
	}
	return strings.TrimSpace(dto.Password.Username) != "" ||
		strings.TrimSpace(dto.Password.Password) != ""
}

// hasUsableTLS reports whether the DTO carries usable TLS material: a CA
// bundle (server verification) and/or a complete cert+key pair (mutual TLS).
// A torn half is rejected earlier by ensureUsableCredential, so by the time
// this runs cert and key are either both present or both absent.
func (dto *credentialSetDTO) hasUsableTLS() bool {
	if dto.TLS == nil {
		return false
	}
	for _, ca := range dto.TLS.CAPEMs {
		if strings.TrimSpace(ca) != "" {
			return true
		}
	}
	hasCert := strings.TrimSpace(dto.TLS.CertPEM) != ""
	hasKey := strings.TrimSpace(dto.TLS.KeyPEM) != ""
	return hasCert && hasKey
}

type Repository struct {
	basePath  string
	namespace string
	clk       clock.Clock
	logger    *slog.Logger
	mu        sync.RWMutex

	// Filesystem seams. Default to the os.* functions; tests override them to
	// simulate a read-only / operator-owned Secret mount without needing a real
	// EROFS filesystem.
	mkdirAll func(string, os.FileMode) error
	stat     func(string) (os.FileInfo, error)
	chmod    func(string, os.FileMode) error

	// readWarnOnce ensures the group/world-readable file WARN is emitted at
	// most once per repository (K8s Secret defaultMode is 0644, so an
	// operator-correct deployment would otherwise log on every read).
	readWarnOnce sync.Once
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

// WithLogger sets the structured logger used for permission-hardening warnings
// (a read-only mount whose permissions cannot be tightened, or a group/world
// readable credential file). nil is ignored; without a logger these conditions
// are handled silently.
func WithLogger(l *slog.Logger) Option {
	return func(r *Repository) {
		if l != nil {
			r.logger = l
		}
	}
}

func New(basePath string, opts ...Option) (*Repository, error) {
	if basePath == "" {
		return nil, fmt.Errorf("basePath is required")
	}

	r := &Repository{
		basePath: basePath,
		clk:      clock.System,
		mkdirAll: os.MkdirAll,
		stat:     os.Stat,
		chmod:    os.Chmod,
	}
	for _, opt := range opts {
		opt(r)
	}

	if err := r.ensureBaseDir(); err != nil {
		return nil, err
	}

	return r, nil
}

// ensureBaseDir creates and hardens the base directory, tolerating an
// operator-owned, read-only Secret mount (F6). K8s mounts Secrets read-only
// (and often with a defaultMode that grants group/other read), so the previous
// unconditional MkdirAll + hard-fail chmod crash-looped the pod on a perfectly
// valid deployment.
//
//   - If MkdirAll fails but the directory already exists, the mount is
//     operator-provisioned and read-only: accept it.
//   - Permission tightening is best-effort — see tightenDirPermissions.
func (r *Repository) ensureBaseDir() error {
	// 0700: the directory tree holds secret material — a group/world listable
	// directory leaks the set of credential names even when the files
	// themselves are 0600.
	if err := r.mkdirAll(r.basePath, 0o700); err != nil {
		info, statErr := r.stat(r.basePath)
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("failed to create base path: %w", err)
		}
		// The directory exists (read-only parent); proceed to best-effort
		// hardening below.
	}
	return r.tightenDirPermissions(r.basePath)
}

// tightenDirPermissions best-effort chmods a pre-existing credentials directory
// to 0700 when its current mode grants any group/other access. MkdirAll does
// not touch the mode of directories that already exist, so without this a
// repository pointed at an old 0755 tree would keep leaking credential names.
//
// F6: when the chmod fails because the mount is read-only or unowned
// (EROFS/EPERM) — an operator-controlled Secret volume — the failure is logged
// at WARN and tolerated rather than returned, so a valid immutable-mount
// deployment does not crash-loop. A genuine, fixable leak (a writable directory
// we own that grants group/other access) is still tightened; a chmod failure
// for any OTHER reason is still a hard error.
func (r *Repository) tightenDirPermissions(dir string) error {
	info, err := r.stat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat base path: %w", err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := r.chmod(dir, 0o700); err != nil {
		if isReadOnlyOrPermError(err) {
			if r.logger != nil {
				r.logger.Warn("credential file store: cannot tighten base directory permissions on a read-only or unowned mount; leaving operator-controlled permissions as-is",
					"path", dir,
					"mode", info.Mode().Perm().String(),
					"error", err,
				)
			}
			return nil
		}
		return fmt.Errorf("failed to restrict base path permissions to 0700: %w", err)
	}
	return nil
}

// isReadOnlyOrPermError reports whether err indicates the filesystem or object
// is read-only or not owned by this process (an operator-controlled mount),
// as opposed to an unexpected I/O failure.
func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, fs.ErrPermission)
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

	// LOW: a credential file that is group/world readable (e.g. a K8s Secret
	// mounted with defaultMode 0644) is a leak the operator should tighten.
	// Warn once — do NOT fail; the file is operator-provisioned and readable.
	r.warnIfWorldReadable(filePath)

	var stored storedCredentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage("failed to parse credentials file").Wrap(err)
	}

	if stored.Credentials == nil {
		// F3: an envelope with no credentials — {"version":1} (key absent) or
		// {"credentials":null} — previously resolved to (nil, nil): the
		// transport then connected anonymously and the poller skipped the nil
		// forever. Treat it as a hard, non-retryable payload error instead.
		return nil, shared.ErrInvalidPayload.WithMessage(
			"credentials file contains no credentials (missing or null \"credentials\" field)")
	}

	// c12: an envelope whose credential set is present but carries neither a
	// usable password nor usable TLS material — {"credentials":{}} or one that
	// only holds whitespace — would resolve to CredentialSet{nil,nil} and let
	// the transport connect with no auth material. Reject a torn or blanked
	// file here so it can never masquerade as a valid anonymous credential.
	if err := ensureUsableCredential(stored.Credentials); err != nil {
		return nil, err
	}

	return stored.Credentials.toDomain(), nil
}

// warnIfWorldReadable emits a one-time WARN when the credential file grants any
// group/other access. Best-effort and silent without a configured logger.
func (r *Repository) warnIfWorldReadable(filePath string) {
	if r.logger == nil {
		return
	}
	info, err := r.stat(filePath)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		r.readWarnOnce.Do(func() {
			r.logger.Warn("credential file store: credential file is group/world accessible; restrict it to 0600 (a Kubernetes Secret defaultMode of 0644 triggers this)",
				"path", filePath,
				"mode", info.Mode().Perm().String(),
			)
		})
	}
}

func (r *Repository) Create(ctx context.Context, uri string, creds *connectivity.CredentialSet) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// c12: never persist a credential set with no usable material — an empty
	// or whitespace-only set would strip auth from any transport that later
	// resolves it. Validate before taking the lock or touching the disk.
	dto := toDTO(creds)
	if err := ensureUsableCredential(dto); err != nil {
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
		Credentials: dto,
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

	// c12: reject a rotation to an empty or whitespace-only credential set so
	// it can never silently replace live credentials with a no-auth no-op.
	// Validate the incoming material before reading or rewriting the file.
	dto := toDTO(creds)
	if err := ensureUsableCredential(dto); err != nil {
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

	stored.Credentials = dto
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
		// file-uri-leak: url.Parse wraps the raw URI (which may embed
		// `user:pass@` userinfo) in a *url.Error that echoes it verbatim into
		// the message. Strip the credential-bearing components before the
		// error reaches any log or caller, matching the SSM and runtime
		// credential paths (shared.RedactURIError -> shared.RedactURI).
		return "", fmt.Errorf("invalid URI: %w", shared.RedactURIError(err))
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
