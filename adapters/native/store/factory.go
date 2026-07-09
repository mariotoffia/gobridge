package nativestore

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory = (*MemoryStoreFactory)(nil)
	_ ports.StoreFactory = (*SQLiteStoreFactory)(nil)
)

// MemoryStoreFactory creates in-memory store instances.
type MemoryStoreFactory struct{}

// NewMemoryStoreFactory creates a MemoryStoreFactory.
func NewMemoryStoreFactory() *MemoryStoreFactory {
	return &MemoryStoreFactory{}
}

// NewLeaseStore creates an in-memory lease store. The in-memory lease keeps
// ownership in a per-process map and cannot coordinate across replicas, so it
// is built ONLY when the operator has acknowledged single-replica operation via
// the "acknowledge_single_replica" config key. Absent, construction fails fast
// here rather than letting a clustered deployment silently split the brain — a
// missing/empty config never wires a lease store at all, so this gate does not
// affect the healthy empty-start path. See finding c10-memlease-split.
func (f *MemoryStoreFactory) NewLeaseStore(_ context.Context, cfg ports.PluginConfig) (ports.LeaseStore, error) {
	if !memoryConfigFrom(cfg).AcknowledgeSingleReplica {
		return nil, fmt.Errorf("nativestore: in-memory lease store requires \"acknowledge_single_replica: true\" — it keeps ownership per-process and cannot coordinate across replicas, so more than one instance silently splits the brain (duplicate / double lease ownership); set the flag to confirm single-replica operation, or use \"dynamodb\" for clustered deployments")
	}
	return memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true)), nil
}

// NewOutboxStore creates an in-memory outbox store.
func (f *MemoryStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return memoryoutbox.NewStore(), nil
}

// NewDLQStore creates an in-memory DLQ store.
func (f *MemoryStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return memorydlq.NewStore(), nil
}

// SQLiteStoreFactory creates SQLite-backed store instances.
// The factory expects a *SQLiteConfig (or SQLiteConfig) PluginConfig
// supplying the database file path.
type SQLiteStoreFactory struct{}

// NewSQLiteStoreFactory creates a SQLiteStoreFactory.
func NewSQLiteStoreFactory() *SQLiteStoreFactory {
	return &SQLiteStoreFactory{}
}

// NewLeaseStore is not supported on SQLite.
func (f *SQLiteStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return nil, fmt.Errorf("nativestore: SQLite lease store is not implemented; use \"memory\" for single-instance or \"dynamodb\" for clustered deployments")
}

// NewOutboxStore creates a SQLite outbox store from the typed config.
// The stale-claim window is threaded into the store so a claim stranded
// by a crashed owner is reclaimed once it goes stale (I1); the typed
// config's stale_claim_duration (when > 0) overrides the runtime-derived
// value, and when both are zero the store stays strictly version-only.
// A non-zero retention overrides the store's default compaction window
// (negative disables compaction).
func (f *SQLiteStoreFactory) NewOutboxStore(_ context.Context, cfg ports.PluginConfig, rt ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	sc, err := requiredSQLiteConfig(cfg)
	if err != nil {
		return nil, err
	}

	staleClaim := rt.StaleClaimDuration
	if sc.StaleClaimDuration > 0 {
		staleClaim = sc.StaleClaimDuration
	}

	opts := []sqliteoutbox.Option{sqliteoutbox.WithStaleClaimDuration(staleClaim)}
	if sc.Retention != 0 {
		opts = append(opts, sqliteoutbox.WithRetention(sc.Retention))
	}
	// Thread the runtime exporter so store-level counters (the fatal
	// storage-fault MetricStoreUnhealthy) are emitted in config-driven
	// deployments. A nil exporter leaves the store's default no-op meter in
	// place (WithMetrics ignores nil), so this never installs a nil exporter.
	if rt.Metrics != nil {
		opts = append(opts, sqliteoutbox.WithMetrics(rt.Metrics))
	}

	return sqliteoutbox.NewStore(sc.Path, opts...) //nolint:wrapcheck // Rule 2/Q3 decorator pass-through; inner sqliteoutbox.NewStore already classifies via mapError.
}

// NewDLQStore creates a SQLite DLQ store from the typed config. A positive
// retention opts the store into a piggybacked purge of expired entries,
// bounding disk growth (zero/unset keeps every entry — the historical default).
func (f *SQLiteStoreFactory) NewDLQStore(_ context.Context, cfg ports.PluginConfig) (ports.DLQStore, error) {
	sc, err := requiredSQLiteConfig(cfg)
	if err != nil {
		return nil, err
	}

	var opts []sqlitedlq.Option
	if sc.Retention > 0 {
		opts = append(opts, sqlitedlq.WithRetention(sc.Retention))
	}

	return sqlitedlq.NewStore(sc.Path, opts...) //nolint:wrapcheck // Rule 2/Q3 decorator pass-through; inner sqlitedlq.NewStore already classifies via mapError.
}

func requiredSQLiteConfig(cfg ports.PluginConfig) (SQLiteConfig, error) {
	sc, ok := sqliteConfigFrom(cfg)
	if !ok {
		return SQLiteConfig{}, fmt.Errorf("nativestore: SQLite store requires a *SQLiteConfig, got %T", cfg)
	}
	if sc.Path == "" {
		return SQLiteConfig{}, fmt.Errorf("nativestore: missing required option \"path\" in SQLite store config")
	}
	return sc, nil
}

// memoryConfigFrom extracts a MemoryConfig from the PluginConfig, returning the
// zero value (AcknowledgeSingleReplica=false) when the config is nil, a typed
// nil, or a different type. The zero value drives NewLeaseStore's fail-closed
// gate, so an absent or mistyped config can never silently build an
// unacknowledged single-replica lease store.
func memoryConfigFrom(cfg ports.PluginConfig) MemoryConfig {
	switch v := cfg.(type) {
	case *MemoryConfig:
		if v != nil {
			return *v
		}
	case MemoryConfig:
		return v
	}
	return MemoryConfig{}
}

func sqliteConfigFrom(cfg ports.PluginConfig) (SQLiteConfig, bool) {
	switch v := cfg.(type) {
	case *SQLiteConfig:
		if v == nil {
			return SQLiteConfig{}, false
		}
		return *v, true
	case SQLiteConfig:
		return v, true
	default:
		return SQLiteConfig{}, false
	}
}
