// Package sqlitemanagedsubscriptions persists exact durable MQTT topic-filter
// history in a dedicated SQLite database.
package sqlitemanagedsubscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed managed-subscription history.
type Store struct{ db *sql.DB }

var _ ports.ManagedSubscriptionStore = (*Store)(nil)

// NewStore opens dbPath with a non-cancelable compatibility context.
// Config-driven construction uses NewStoreContext so build cancellation reaches
// every blocking SQLite operation.
func NewStore(dbPath string) (*Store, error) {
	return NewStoreContext(context.Background(), dbPath)
}

// NewStoreContext opens dbPath, enforces owner-only filesystem permissions,
// applies durable SQLite settings, and creates the minimal exact-filter schema.
func NewStoreContext(ctx context.Context, dbPath string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, shared.ErrInvalidConfig.WithMessage("managed subscription SQLite path is required")
	}

	acl, err := prepareSQLiteACL(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer acl.close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, unavailable("open managed subscription store", err)
	}
	db.SetMaxOpenConns(1)
	closeWithACL := func() {
		_ = db.Close()
		_ = acl.secureCreatedFiles()
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			closeWithACL()
			return nil, unavailable("configure managed subscription store", err)
		}
	}
	store := &Store{db: db}
	if err := store.init(ctx); err != nil {
		closeWithACL()
		return nil, err
	}
	if err := acl.secureCreatedFiles(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

type sqliteACL struct {
	path        string
	base        string
	dirFD       int
	dirDev      uint64
	dirIno      uint64
	preexisting map[string]bool
}

func (a *sqliteACL) close() {
	if a != nil && a.dirFD >= 0 {
		_ = unix.Close(a.dirFD)
		a.dirFD = -1
	}
}

func prepareSQLiteACL(ctx context.Context, dbPath string) (*sqliteACL, error) {
	if err := validateSQLitePath(dbPath); err != nil {
		return nil, err
	}
	parent := filepath.Dir(dbPath)
	dirFD, stat, err := openSecureSQLiteParent(ctx, parent)
	if err != nil {
		return nil, err
	}
	acl := &sqliteACL{
		path: dbPath, base: filepath.Base(dbPath), dirFD: dirFD,
		dirDev: uint64(stat.Dev), dirIno: uint64(stat.Ino),
		preexisting: make(map[string]bool, 4),
	}
	failed := true
	defer func() {
		if failed {
			acl.close()
		}
	}()

	for _, name := range acl.names() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		exists, inspectErr := acl.inspectRelative(name, false)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if exists {
			acl.preexisting[name] = true
			continue
		}
		if name != acl.base {
			continue
		}
		fd, createErr := unix.Openat(acl.dirFD, name,
			unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if createErr != nil {
			return nil, unavailable("create managed subscription SQLite database", createErr)
		}
		if chmodErr := unix.Fchmod(fd, 0o600); chmodErr != nil {
			_ = unix.Close(fd)
			return nil, unavailable("secure managed subscription SQLite database", chmodErr)
		}
		if validateErr := validateSQLiteFileFD(fd, dbPath); validateErr != nil {
			_ = unix.Close(fd)
			return nil, validateErr
		}
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, unavailable("close new managed subscription SQLite database", closeErr)
		}
	}
	failed = false
	return acl, nil
}

func validateSQLitePath(dbPath string) error {
	if dbPath == "" {
		return shared.ErrInvalidConfig.WithMessage("managed subscription SQLite path is required")
	}
	if !filepath.IsAbs(dbPath) || filepath.Clean(dbPath) != dbPath ||
		strings.HasPrefix(strings.ToLower(dbPath), "file:") ||
		strings.ContainsAny(dbPath, "?#") || strings.ContainsRune(dbPath, 0) {
		return shared.ErrInvalidConfig.WithMessage(
			"managed subscription SQLite path must be a plain absolute clean filesystem path without URI or query syntax")
	}
	if filepath.Base(dbPath) == "." || filepath.Base(dbPath) == string(filepath.Separator) {
		return shared.ErrInvalidConfig.WithMessage("managed subscription SQLite path must name a database file")
	}
	return nil
}

func openSecureSQLiteParent(ctx context.Context, parent string) (int, unix.Stat_t, error) {
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, unavailable("open managed subscription SQLite root", err)
	}
	components := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	// Darwin exposes /var as a root-owned compatibility symlink to /private/var.
	// Walk its immutable canonical target descriptor-relatively; all service-local
	// symlink components remain rejected by O_NOFOLLOW below.
	if runtime.GOOS == "darwin" && len(components) > 0 && components[0] == "var" {
		components = append([]string{"private", "var"}, components[1:]...)
	}
	for i, component := range components {
		if component == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, err
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		created := false
		if openErr == unix.ENOENT {
			if mkdirErr := unix.Mkdirat(fd, component, 0o700); mkdirErr != nil {
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, unavailable("create managed subscription SQLite parent", mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			created = true
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("managed subscription SQLite parent component %q must be a non-symlink directory", component)).Wrap(openErr)
		}
		_ = unix.Close(fd)
		fd = next
		if created {
			if chmodErr := unix.Fchmod(fd, 0o700); chmodErr != nil {
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, unavailable("secure managed subscription SQLite parent", chmodErr)
			}
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, unavailable("inspect managed subscription SQLite parent", statErr)
		}
		final := i == len(components)-1
		if validateErr := validateSQLiteDirectory(component, stat, final); validateErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, validateErr
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, unavailable("inspect managed subscription SQLite parent", err)
	}
	return fd, stat, nil
}

func validateSQLiteDirectory(component string, stat unix.Stat_t, final bool) error {
	perm := uint32(stat.Mode) & 0o7777
	uid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != uid {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite parent component %q has an unsafe owner", component))
	}
	if final {
		if stat.Uid != uid || perm != 0o700 {
			return shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("managed subscription SQLite final parent %q must be owned by the process user with permissions 0700", component))
		}
		return nil
	}
	if perm&0o022 != 0 && (perm&0o1000 == 0 || stat.Uid != 0) {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite parent component %q is writable by group or other", component))
	}
	return nil
}

func (a *sqliteACL) names() []string {
	return []string{a.base, a.base + "-wal", a.base + "-shm", a.base + "-journal"}
}

func (a *sqliteACL) inspectRelative(name string, allowTighten bool) (bool, error) {
	fd, err := unix.Openat(a.dirFD, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == unix.ENOENT {
		return false, nil
	}
	if err != nil {
		return false, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q must be regular and must not be a symlink", filepath.Join(filepath.Dir(a.path), name))).Wrap(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := validateSQLiteFileFD(fd, filepath.Join(filepath.Dir(a.path), name)); err != nil {
		if !allowTighten {
			return false, err
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return false, err
		}
		if chmodErr := unix.Fchmod(fd, 0o600); chmodErr != nil {
			return false, unavailable("secure managed subscription SQLite files", chmodErr)
		}
		if verifyErr := validateSQLiteFileFD(fd, filepath.Join(filepath.Dir(a.path), name)); verifyErr != nil {
			return false, verifyErr
		}
	}
	return true, nil
}

func validateSQLiteFileFD(fd int, path string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return unavailable("inspect managed subscription SQLite files", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q must be an owner-controlled regular file", path))
	}
	if stat.Mode&0o777 != 0o600 {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q has insecure permissions %04o; require 0600", path, stat.Mode&0o777))
	}
	return nil
}

func (a *sqliteACL) verifyHeldParent() error {
	fd, stat, err := openSecureSQLiteParent(context.Background(), filepath.Dir(a.path))
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if uint64(stat.Dev) != a.dirDev || uint64(stat.Ino) != a.dirIno {
		return shared.ErrInvalidConfig.WithMessage("managed subscription SQLite parent changed during initialization")
	}
	return nil
}

func (a *sqliteACL) secureCreatedFiles() error {
	if err := a.verifyHeldParent(); err != nil {
		return err
	}
	for _, name := range a.names() {
		allowTighten := !a.preexisting[name]
		if _, err := a.inspectRelative(name, allowTighten); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) init(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS managed_subscription_baselines (
    storage_identity TEXT PRIMARY KEY NOT NULL
);
CREATE TABLE IF NOT EXISTS managed_subscription_filters (
    storage_identity TEXT NOT NULL,
    filter TEXT NOT NULL,
    PRIMARY KEY (storage_identity, filter),
    FOREIGN KEY (storage_identity) REFERENCES managed_subscription_baselines(storage_identity) ON DELETE CASCADE
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return unavailable("initialize managed subscription store", err)
	}
	return nil
}

// List returns an ordered snapshot or ErrNotFound when no baseline exists.
func (s *Store) List(ctx context.Context, storageIdentity string) ([]string, error) {
	if err := validate(storageIdentity, nil); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT f.filter
FROM managed_subscription_baselines AS b
LEFT JOIN managed_subscription_filters AS f ON f.storage_identity = b.storage_identity
WHERE b.storage_identity = ?
ORDER BY f.filter ASC`, storageIdentity)
	if err != nil {
		return nil, unavailable("list managed subscriptions", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	filters := make([]string, 0)
	for rows.Next() {
		found = true
		var filter sql.NullString
		if err := rows.Scan(&filter); err != nil {
			return nil, unavailable("scan managed subscriptions", err)
		}
		if filter.Valid {
			filters = append(filters, filter.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable("read managed subscriptions", err)
	}
	if !found {
		return nil, shared.ErrNotFound.WithMessage("managed subscription baseline not found")
	}
	return filters, nil
}

// Remember establishes a baseline and atomically adds exact filters.
func (s *Store) Remember(ctx context.Context, storageIdentity string, filters []string) error {
	values, err := normalized(storageIdentity, filters)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable("begin managed subscription remember", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_subscription_baselines(storage_identity) VALUES (?) ON CONFLICT(storage_identity) DO NOTHING`, storageIdentity); err != nil {
		return unavailable("establish managed subscription baseline", err)
	}
	for _, filter := range values {
		if _, err = tx.ExecContext(ctx, `INSERT INTO managed_subscription_filters(storage_identity, filter) VALUES (?, ?) ON CONFLICT(storage_identity, filter) DO NOTHING`, storageIdentity, filter); err != nil {
			return unavailable("remember managed subscription", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return unavailable("commit managed subscription remember", err)
	}
	return nil
}

// Forget atomically removes exact filters while retaining the baseline.
func (s *Store) Forget(ctx context.Context, storageIdentity string, filters []string) error {
	values, err := normalized(storageIdentity, filters)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable("begin managed subscription forget", err)
	}
	defer func() { _ = tx.Rollback() }()
	var present int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM managed_subscription_baselines WHERE storage_identity = ?`, storageIdentity).Scan(&present); err != nil {
		if err == sql.ErrNoRows {
			return shared.ErrNotFound.WithMessage("managed subscription baseline not found")
		}
		return unavailable("read managed subscription baseline", err)
	}
	for _, filter := range values {
		if _, err = tx.ExecContext(ctx, `DELETE FROM managed_subscription_filters WHERE storage_identity = ? AND filter = ?`, storageIdentity, filter); err != nil {
			return unavailable("forget managed subscription", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return unavailable("commit managed subscription forget", err)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return unavailable("close managed subscription store", err)
	}
	return nil
}

func normalized(identity string, filters []string) ([]string, error) {
	if err := validate(identity, filters); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(filters))
	values := make([]string, 0, len(filters))
	for _, filter := range filters {
		if _, ok := seen[filter]; ok {
			continue
		}
		seen[filter] = struct{}{}
		values = append(values, filter)
	}
	sort.Strings(values)
	return values, nil
}

func validate(identity string, filters []string) error {
	if identity == "" {
		return shared.ErrInvalidConfig.WithMessage("managed subscription storage identity is required")
	}
	for _, filter := range filters {
		if filter == "" {
			return shared.ErrInvalidConfig.WithMessage("managed subscription filter must not be empty")
		}
	}
	return nil
}

func unavailable(message string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return shared.ErrUnavailable.WithMessage(message).Wrap(err)
}
