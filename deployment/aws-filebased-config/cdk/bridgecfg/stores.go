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

// WithSQLiteManagedSubscriptions installs a SQLite-backed exact-filter
// history store at path. Subsequent calls replace the previous managed-
// subscription store.
func (b *Builder) WithSQLiteManagedSubscriptions(path string) *Builder {
	sc, ok := b.sqliteStore("managed_subscriptions", path)
	if !ok {
		return b
	}
	b.cfg.Stores.ManagedSubscriptions = sc
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

// WithMemoryOutbox installs the in-memory outbox store. The memory
// store is intended for single-bridge deployments and tests; cluster
// mode requires a persistent backend (SQLite or DynamoDB). Calls
// replace any previously installed outbox.
func (b *Builder) WithMemoryOutbox() *Builder {
	b.cfg.Stores.Outbox = memoryStore()
	return b
}

// WithMemoryLease installs the in-memory lease store. Same single-
// bridge caveats as WithMemoryOutbox apply — and because an in-memory
// lease keeps ownership per-process and cannot coordinate across
// replicas, the emitted config carries acknowledge_single_replica: true
// so the runtime accepts it (MemoryStoreFactory.NewLeaseStore fails
// closed otherwise). Keep such a deployment at exactly one replica; use
// a DynamoDB lease for clustered failover.
func (b *Builder) WithMemoryLease() *Builder {
	sc := &ports.StoreConfig{Type: nativestore.MemoryKind}
	sc.SetDecoded(nativestore.MemoryConfig{AcknowledgeSingleReplica: true}, nil)
	b.cfg.Stores.Lease = sc
	return b
}

// WithMemoryDLQ installs the in-memory DLQ store. Same single-
// bridge caveats as WithMemoryOutbox apply.
func (b *Builder) WithMemoryDLQ() *Builder {
	b.cfg.Stores.DLQ = memoryStore()
	return b
}

// memoryStore is the shared assembly path for the in-memory OUTBOX and DLQ
// stores. Both hold their records in the process heap while a successful write
// already permits the runtime to settle the source, so the emitted config
// carries acknowledge_volatile: true — MemoryStoreFactory.NewOutboxStore and
// NewDLQStore fail closed otherwise. Choosing WithMemoryOutbox/WithMemoryDLQ in
// the CDK IS that acknowledgement; the lease store's own
// acknowledge_single_replica is set on its separate path in WithMemoryLease.
func memoryStore() *ports.StoreConfig {
	sc := &ports.StoreConfig{Type: nativestore.MemoryKind}
	sc.SetDecoded(nativestore.MemoryConfig{AcknowledgeVolatile: true}, nil)
	return sc
}

// sqliteStore is the shared assembly path for the four SQLite store
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
