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
	"sort"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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

	var acl *sqliteACL
	if dbPath != ":memory:" {
		var err error
		acl, err = prepareSQLiteACL(ctx, dbPath)
		if err != nil {
			return nil, err
		}
	}
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
		if acl != nil {
			_ = acl.secureCreatedFiles()
		}
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
	if acl != nil {
		if err := acl.secureCreatedFiles(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return store, nil
}

type sqliteACL struct {
	path        string
	preexisting map[string]bool
}

func prepareSQLiteACL(ctx context.Context, dbPath string) (*sqliteACL, error) {
	parent := filepath.Dir(dbPath)
	if err := ensureSQLiteParent(ctx, parent); err != nil {
		return nil, err
	}
	acl := &sqliteACL{path: dbPath, preexisting: make(map[string]bool, 3)}
	for _, path := range acl.paths() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			acl.preexisting[path] = true
			if err := validateOwnerOnlyRegular(path, info); err != nil {
				return nil, err
			}
		case errors.Is(err, os.ErrNotExist):
			// SQLite creates WAL/SHM itself. The main database is pre-created with
			// O_EXCL so its permissions never depend on process umask.
			if path != dbPath {
				continue
			}
			file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if createErr != nil {
				return nil, unavailable("create managed subscription SQLite database", createErr)
			}
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				_ = file.Close()
				return nil, unavailable("secure managed subscription SQLite database", chmodErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				return nil, unavailable("close new managed subscription SQLite database", closeErr)
			}
		default:
			return nil, unavailable("inspect managed subscription SQLite files", err)
		}
	}
	return acl, nil
}

func ensureSQLiteParent(ctx context.Context, parent string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err == nil {
		if !info.IsDir() {
			return shared.ErrInvalidConfig.WithMessage("managed subscription SQLite parent is not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return unavailable("inspect managed subscription SQLite parent", err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return unavailable("create managed subscription SQLite parent", err)
	}
	// MkdirAll is umask-sensitive. This directory did not exist before this
	// adapter call, so it is adapter-owned and may be tightened deterministically.
	if err := os.Chmod(parent, 0o700); err != nil {
		return unavailable("secure managed subscription SQLite parent", err)
	}
	return nil
}

func (a *sqliteACL) paths() []string {
	return []string{a.path, a.path + "-wal", a.path + "-shm"}
}

func (a *sqliteACL) secureCreatedFiles() error {
	for _, path := range a.paths() {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return unavailable("inspect managed subscription SQLite files", err)
		}
		if a.preexisting[path] {
			if err := validateOwnerOnlyRegular(path, info); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("managed subscription SQLite file %q must be regular and must not be a symlink", path))
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return unavailable("secure managed subscription SQLite files", err)
		}
		info, err = os.Lstat(path)
		if err != nil {
			return unavailable("verify managed subscription SQLite files", err)
		}
		if err := validateOwnerOnlyRegular(path, info); err != nil {
			return err
		}
	}
	return nil
}

func validateOwnerOnlyRegular(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("managed subscription SQLite file %q must be regular and must not be a symlink", path))
	}
	if info.Mode().Perm() != 0o600 {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("managed subscription SQLite file %q has insecure permissions %04o; require 0600", path, info.Mode().Perm()))
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
