package sqliteoutbox

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"

	sqlite3 "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// SQLite error classification helpers.
//
// We use modernc.org/sqlite (the only SQLite driver wired into this
// module's go.mod). Its `*sqlite.Error` carries the extended SQLite
// result code via Code(); we detect the relevant classes via
// `errors.As` and fall back to string matching only when the typed
// assertion misses (e.g. an error that has been flattened across a
// boundary).

func isUniqueViolation(err error) bool {
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3lib.SQLITE_CONSTRAINT_PRIMARYKEY,
			sqlite3lib.SQLITE_CONSTRAINT_UNIQUE:
			return true
		}
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isBusy(err error) bool {
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3lib.SQLITE_BUSY,
			sqlite3lib.SQLITE_BUSY_RECOVERY,
			sqlite3lib.SQLITE_BUSY_SNAPSHOT,
			sqlite3lib.SQLITE_BUSY_TIMEOUT,
			sqlite3lib.SQLITE_LOCKED:
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

func isIOErr(err error) bool {
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		c := serr.Code()
		// Primary code SQLITE_IOERR (10) and any extended IO-error code
		// share the lower 8 bits of the primary code.
		if c == sqlite3lib.SQLITE_IOERR || (c&0xFF) == sqlite3lib.SQLITE_IOERR {
			return true
		}
	}
	return false
}

// mapError classifies a SQLite error per the error-wrapping policy
// SQLite mapping table.
//
// Per policy Rule 1 (`_design/error-wrapping-policy.adoc:100-104`),
// `context.Canceled` and `context.DeadlineExceeded` are canonical
// sentinels and are returned UNCHANGED (identity-equal) so callers can
// match them with `errors.Is`. They are NEVER reclassified as
// `shared.ErrTimeout` or `shared.ErrUnavailable`.
//
// Caller-handled outcomes (UNIQUE constraint, sql.ErrNoRows when the
// caller wants a typed not-found with attached kvs) may be inspected
// via the helpers above and short-circuited at the call site so the
// outbox-specific semantics (ErrDuplicateRecord) are preserved.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// Rule 1: ctx errors pass through verbatim. Do not reclassify.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	if errors.Is(err, sql.ErrNoRows) {
		return shared.ErrNotFound.Wrap(err)
	}
	if isUniqueViolation(err) {
		return shared.ErrDuplicateRecord.Wrap(err)
	}
	if isBusy(err) {
		return shared.ErrThrottled.Wrap(err)
	}
	if errors.Is(err, io.EOF) || isIOErr(err) {
		return shared.ErrConnectionLost.Wrap(err)
	}
	return shared.ErrUnavailable.Wrap(err)
}

// wrapErr is the canonical call-site helper for this package. It
// preserves canonical context sentinels (returned identity-equal) and
// otherwise classifies via mapError, attaching the supplied message
// and key/value annotations to the resulting *shared.BridgeError.
//
// kvs must be a sequence of (string-key, value) pairs. An odd-length
// or non-string key is silently ignored to keep error paths panic-free.
func wrapErr(err error, msg string, kvs ...any) error {
	mapped := mapError(err)
	if mapped == nil {
		return nil
	}
	be, ok := mapped.(*shared.BridgeError)
	if !ok {
		// ctx sentinel — return verbatim per policy Rule 1.
		return mapped
	}
	if msg != "" {
		be = be.WithMessage(msg)
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			continue
		}
		be = be.With(key, kvs[i+1])
	}
	return be
}
