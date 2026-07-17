//go:build windows

package sqlitemanagedsubscriptions

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// sqliteACL is intentionally inert on Windows. New construction fails before
// SQLite is opened because this adapter requires descriptor-relative no-follow
// creation and validation for the database and every sidecar; the repository
// does not currently have equivalent Windows handle-relative ACL primitives.
type sqliteACL struct{}

func prepareSQLiteACL(ctx context.Context, dbPath string) (*sqliteACL, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSQLitePath(dbPath); err != nil {
		return nil, err
	}
	return nil, shared.ErrInvalidConfig.WithMessage(
		"managed subscription SQLite store is unavailable on Windows: secure no-follow file and sidecar creation is not implemented")
}

func (*sqliteACL) close()                    {}
func (*sqliteACL) secureCreatedFiles() error { return nil }
