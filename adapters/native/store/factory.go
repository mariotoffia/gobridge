package nativestore

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory                    = (*MemoryStoreFactory)(nil)
	_ ports.StoreFactory                    = (*SQLiteStoreFactory)(nil)
	_ ports.ManagedSubscriptionStoreFactory = (*SQLiteStoreFactory)(nil)
	_ ports.CrashDurableStoreFactory        = (*MemoryStoreFactory)(nil)
	_ ports.CrashDurableStoreFactory        = (*SQLiteStoreFactory)(nil)
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
// affect the healthy empty-start path.
func (f *MemoryStoreFactory) NewLeaseStore(_ context.Context, cfg ports.PluginConfig) (ports.LeaseStore, error) {
	if !memoryConfigFrom(cfg).AcknowledgeSingleReplica {
		return nil, shared.ErrInvalidConfig.WithMessage(
			"nativestore: in-memory lease store requires \"acknowledge_single_replica: true\" — it keeps " +
				"ownership per-process and cannot coordinate across replicas, so more than one instance " +
				"silently splits the brain (duplicate / double lease ownership); set the flag to confirm " +
				"single-replica operation, or use \"dynamodb\" for clustered deployments")
	}
	return memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true)), nil
}

// NewOutboxStore creates an in-memory outbox store. The records live in the
// process heap, so a restart or crash loses every one of them while a
// successful Persist has already permitted the runtime to settle the SOURCE —
// the outbox port's crash-durable success boundary is deliberately NOT met.
// Construction therefore requires "acknowledge_volatile: true", exactly as the
// lease store requires its own acknowledgement: an operator who never states
// the tradeoff cannot end up running a persist-before-ack route on a store that
// silently drops acknowledged work.
func (f *MemoryStoreFactory) NewOutboxStore(_ context.Context, cfg ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	if err := requireVolatileAcknowledged(cfg, "outbox", "accepted work already acknowledged upstream"); err != nil {
		return nil, err
	}
	return memoryoutbox.NewStore(), nil
}

// NewDLQStore creates an in-memory DLQ store. It is gated on the same
// "acknowledge_volatile: true" as the in-memory outbox and for the same reason:
// entries live in the process heap, so a restart erases the terminal evidence
// that a message existed and was given up on — the only record of the loss.
func (f *MemoryStoreFactory) NewDLQStore(_ context.Context, cfg ports.PluginConfig) (ports.DLQStore, error) {
	if err := requireVolatileAcknowledged(cfg, "dlq", "terminal evidence of dropped messages"); err != nil {
		return nil, err
	}
	return memorydlq.NewStore(), nil
}

// IsCrashDurable reports that in-memory stores do NOT survive the process, so
// composition can reject the pairings that depend on durability and gate the
// ones that merely trade it away.
func (f *MemoryStoreFactory) IsCrashDurable() bool { return false }

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
	return nil, shared.ErrNotSupported.WithMessage(
		"nativestore: SQLite lease store is not implemented; use \"dynamodb\", or \"memory\" for a single " +
			"instance whose outbox and DLQ are ALSO in-memory — an in-memory lease renumbers its fencing " +
			"versions from zero on every restart and cannot back a SQLite or DynamoDB outbox, whose durable " +
			"partition fence would then reject every claim")
}

// IsCrashDurable reports that SQLite-backed stores survive the process: the
// records are on disk and readable by the process that replaces this one.
func (f *SQLiteStoreFactory) IsCrashDurable() bool { return true }

// NewOutboxStore creates a SQLite outbox store from the typed config.
// The stale-claim window is threaded into the store so a claim stranded
// by a crashed owner is reclaimed once it goes stale; the typed
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

// NewManagedSubscriptionStore creates a dedicated exact-filter history.
func (f *SQLiteStoreFactory) NewManagedSubscriptionStore(ctx context.Context, cfg ports.PluginConfig) (ports.ManagedSubscriptionStore, error) { //nolint:ireturn // Factory port intentionally returns the narrow role interface.
	sc, err := requiredSQLiteConfig(cfg)
	if err != nil {
		return nil, err
	}
	store, err := sqlitemanagedsubscriptions.NewStoreContext(ctx, sc.Path)
	if err != nil {
		return nil, err //nolint:wrapcheck // adapter classifies errors
	}
	return store, nil
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

// requireVolatileAcknowledged enforces the in-memory outbox/DLQ opt-in. role
// names the store role and atRisk names, in operator terms, what the process
// loses on restart, so the error says what is at stake rather than only which
// key is missing.
func requireVolatileAcknowledged(cfg ports.PluginConfig, role, atRisk string) error {
	if memoryConfigFrom(cfg).AcknowledgeVolatile {
		return nil
	}
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"nativestore: in-memory %s store requires \"acknowledge_volatile: true\" — it keeps records in the "+
			"process heap, so a restart, crash, or OOM kill loses %s; set the flag to confirm the loss is "+
			"acceptable, or use \"sqlite\"/\"dynamodb\" when the records must survive the process", role, atRisk))
}

func requiredSQLiteConfig(cfg ports.PluginConfig) (SQLiteConfig, error) {
	sc, ok := sqliteConfigFrom(cfg)
	if !ok {
		return SQLiteConfig{}, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("nativestore: SQLite store requires a *SQLiteConfig, got %T", cfg))
	}
	if sc.Path == "" {
		return SQLiteConfig{}, shared.ErrInvalidConfig.WithMessage(
			"nativestore: missing required option \"path\" in SQLite store config")
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
