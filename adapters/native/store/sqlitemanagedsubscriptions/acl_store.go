// Package sqlitemanagedsubscriptions persists exact durable MQTT topic-filter
// history in a dedicated SQLite database.
package sqlitemanagedsubscriptions

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed managed-subscription history.
type Store struct{ db *sql.DB }

var _ ports.ManagedSubscriptionStore = (*Store)(nil)

// NewStore opens dbPath, applies durable SQLite settings, and creates the
// minimal baseline and exact-filter tables.
func NewStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, shared.ErrInvalidConfig.WithMessage("managed subscription SQLite path is required")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, unavailable("open managed subscription store", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			_ = db.Close()
			return nil, unavailable("configure managed subscription store", err)
		}
	}
	store := &Store{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
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
