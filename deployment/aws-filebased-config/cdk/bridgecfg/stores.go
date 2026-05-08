package bridgecfg

import (
	"fmt"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

// WithSQLiteOutbox installs a SQLite-backed outbox store at path.
// Subsequent calls replace the previous outbox — the bridge runtime
// supports exactly one outbox store per instance.
func (b *Builder) WithSQLiteOutbox(path string) *Builder {
	sc, ok := b.sqliteStore("outbox", path)
	if !ok {
		return b
	}
	b.cfg.Stores.Outbox = sc
	return b
}

// WithSQLiteLease installs a SQLite-backed lease store at path.
func (b *Builder) WithSQLiteLease(path string) *Builder {
	sc, ok := b.sqliteStore("lease", path)
	if !ok {
		return b
	}
	b.cfg.Stores.Lease = sc
	return b
}

// WithSQLiteDLQ installs a SQLite-backed DLQ store at path.
func (b *Builder) WithSQLiteDLQ(path string) *Builder {
	sc, ok := b.sqliteStore("dlq", path)
	if !ok {
		return b
	}
	b.cfg.Stores.DLQ = sc
	return b
}

// sqliteStore is the shared assembly path for the three SQLite store
// methods. It validates the path through the adapter's own Validate
// so the builder rejects empty paths with the same wording the
// runtime would emit at startup.
func (b *Builder) sqliteStore(role, path string) (*ports.StoreConfig, bool) {
	cfg := &nativestore.SQLiteConfig{Path: path}
	if err := cfg.Validate(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: sqlite %s store: %w", role, err))
		return nil, false
	}
	sc := &ports.StoreConfig{Type: nativestore.SQLiteKind}
	sc.SetDecoded(cfg, nil)
	return sc, true
}
