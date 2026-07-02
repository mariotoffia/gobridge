package sqlitedlq

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"

	// Driver registration. The blank import lives in this ACL file so
	// it never leaks into the port-side dlq.go.
	_ "modernc.org/sqlite"
)

// sqlSession is the unexported wrapper around the SDK's *sql.DB
// handle. It is the lifecycle/orchestration half of the SQLite ACL:
// every database/sql call the adapter makes flows through a method on
// this type. The port-side Store holds a *sqlSession by pointer and
// never imports database/sql itself.
type sqlSession struct {
	db *sql.DB
}

// openSession opens (or creates) the database at path, applies the
// durability/concurrency pragmas, and runs the idempotent schema migration.
//
// The connection pool is capped at a single open connection: modernc.org/sqlite
// gives every *sql.Conn its own private database for ":memory:" paths (a wider
// pool would fracture an in-memory store), and on a file database it serialises
// writers so in-process goroutines never race into SQLITE_BUSY (I2).
//
// ponytail: single-writer ceiling — sufficient for the single-process
// deployments this DLQ store targets; a read-heavy deployment would add a
// separate read-only pool over the WAL. See I2.
func openSession(path string) (*sqlSession, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, wrapErr(err, "sqlitedlq: open", "path", path)
	}

	db.SetMaxOpenConns(1)

	// busy_timeout must be armed BEFORE journal_mode=WAL: converting a
	// rollback-journal file to WAL takes an exclusive lock, and without the
	// timeout that first conversion fails fast with SQLITE_BUSY under
	// concurrent opens of a not-yet-WAL file. Arming it first makes the driver
	// block-and-retry up to the timeout — the retry policy for cross-process
	// contention on a file database, including the initial WAL conversion (I2).
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, wrapErr(err, "sqlitedlq: pragma", "path", path, "pragma", pragma)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqlitedlq: migrate", "path", path)
	}

	return &sqlSession{db: db}, nil
}

// close releases the underlying *sql.DB.
func (s *sqlSession) close() error {
	if err := s.db.Close(); err != nil {
		return wrapErr(err, "sqlitedlq: close")
	}
	return nil
}

// write inserts a single DLQ entry. Duplicates surface as
// shared.ErrDuplicateRecord.
func (s *sqlSession) write(ctx context.Context, entry routing.DLQEntry) error {
	envJSON, err := json.Marshal(entry.Snapshot())
	if err != nil {
		return fmt.Errorf("sqlitedlq: marshal envelope: %w", err)
	}

	_, err = s.db.ExecContext(ctx, insertDLQSQL,
		entry.ID(), entry.RouteID(), entry.BindingID(), entry.SessionID(), entry.SourceID(),
		entry.CorrelationID(), entry.Reason(), entry.Category(), entry.ErrorCode(), entry.LastError(),
		string(envJSON), entry.FailedAt().UnixMilli(), entry.Attempts(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return shared.ErrDuplicateRecord.With("entryID", entry.ID())
		}
		return wrapErr(err, "sqlitedlq: write",
			"entryID", entry.ID(), "routeID", entry.RouteID())
	}
	return nil
}

// list returns DLQ entries matching the supplied filter.
func (s *sqlSession) list(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	query, args := listSQL(filter)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr(err, "sqlitedlq: list",
			"routeID", filter.RouteID, "category", filter.Category)
	}
	defer func() { _ = rows.Close() }()

	return scanDLQEntries(rows)
}

// get returns the entry with id or shared.ErrNotFound.
func (s *sqlSession) get(ctx context.Context, id string) (routing.DLQEntry, error) {
	rows, err := s.db.QueryContext(ctx, selectByIDSQL, id)
	if err != nil {
		return routing.DLQEntry{}, wrapErr(err, "sqlitedlq: get", "entryID", id)
	}
	defer func() { _ = rows.Close() }()

	entries, err := scanDLQEntries(rows)
	if err != nil {
		return routing.DLQEntry{}, err
	}
	if len(entries) == 0 {
		return routing.DLQEntry{}, shared.ErrNotFound.
			WithMessage("dlq entry not found").
			With("entryID", id)
	}
	return entries[0], nil
}

// delete removes the entries with the supplied ids.
func (s *sqlSession) delete(ctx context.Context, ids []string) (int, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	res, err := s.db.ExecContext(ctx, deleteByIDsSQL(len(ids)), args...)
	if err != nil {
		return 0, wrapErr(err, "sqlitedlq: delete", "recordCount", len(ids))
	}

	n, _ := res.RowsAffected()
	return int(n), nil
}

// deleteByFilter removes every entry matching the filter.
func (s *sqlSession) deleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	query, args := deleteByFilterSQL(filter)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, wrapErr(err, "sqlitedlq: delete by filter",
			"routeID", filter.RouteID, "category", filter.Category)
	}

	n, _ := res.RowsAffected()
	return int(n), nil
}

// purge deletes every entry whose failed_at is strictly before before.
func (s *sqlSession) purge(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, purgeBeforeSQL, before.UnixMilli())
	if err != nil {
		return 0, wrapErr(err, "sqlitedlq: purge")
	}

	n, _ := res.RowsAffected()
	return int(n), nil
}
