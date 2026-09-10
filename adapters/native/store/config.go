package nativestore

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contracts.
var (
	_ ports.PluginConfig    = (*MemoryConfig)(nil)
	_ ports.PluginConfig    = (*SQLiteConfig)(nil)
	_ ports.FreezableConfig = (*SQLiteConfig)(nil)
)

// MemoryKind is the registry discriminator for the in-memory store.
const MemoryKind = "memory"

// SQLiteKind is the registry discriminator for the SQLite store.
const SQLiteKind = "sqlite"

// MemoryConfig is the typed PluginConfig for the in-memory store.
type MemoryConfig struct {
	// AcknowledgeSingleReplica must be set to true to build the in-memory
	// LEASE store; it has no effect on the in-memory outbox or DLQ stores.
	// The in-memory lease keeps ownership in a per-process map and cannot
	// coordinate across replicas, so running it behind more than one instance
	// silently splits the brain (every replica believes it owns the same
	// exclusive session). gobridge cannot observe the deployment's replica
	// count, so the operator must explicitly acknowledge single-replica
	// operation. Absent (false), MemoryStoreFactory.NewLeaseStore fails fast
	// at construction. Use the "dynamodb" store for clustered deployments.
	AcknowledgeSingleReplica bool `mapstructure:"acknowledge_single_replica" yaml:"acknowledge_single_replica" json:"acknowledge_single_replica"`

	// AcknowledgeVolatile must be set to true to build the in-memory OUTBOX or
	// DLQ store; it has no effect on the in-memory lease store. Both keep their
	// records in the process heap, so a restart, a crash, or an OOM kill loses
	// them entirely — yet a nil result from either permits the runtime to settle
	// the SOURCE. On a persist-before-ack route that means accepted work is
	// acknowledged upstream and then vanishes, and terminal DLQ evidence of
	// dropped messages disappears with it. gobridge cannot tell a scratch
	// development bridge from a production one, so the operator must state that
	// losing both on restart is acceptable. Absent (false),
	// MemoryStoreFactory.NewOutboxStore and NewDLQStore fail fast at
	// construction rather than let a production-looking route silently lose
	// work. Use "sqlite" or "dynamodb" when the records must survive the
	// process.
	AcknowledgeVolatile bool `mapstructure:"acknowledge_volatile" yaml:"acknowledge_volatile" json:"acknowledge_volatile"`
}

// Kind reports the registry discriminator.
func (MemoryConfig) Kind() string { return MemoryKind }

// Validate is a no-op: both acknowledgements are per-ROLE gates enforced at
// construction (MemoryStoreFactory.NewLeaseStore, NewOutboxStore, NewDLQStore),
// because one MemoryConfig configures every role and each flag is meaningless
// for the roles it does not guard — a lease-only config must validate without
// acknowledge_volatile, and an outbox-only config without
// acknowledge_single_replica.
func (MemoryConfig) Validate() error { return nil }

// SQLiteConfig is the typed PluginConfig for the SQLite-backed
// outbox/DLQ/managed-subscription stores.
type SQLiteConfig struct {
	// Path is the SQLite database file path. ":memory:" selects an
	// in-process database for roles that permit it. Managed subscriptions are
	// durable and require a plain absolute, clean filesystem path.
	Path string `mapstructure:"path" yaml:"path" json:"path"`

	// StaleClaimDuration (outbox only) overrides the runtime-derived
	// stale-claim reclaim window: how long a claim stranded by a
	// crashed same-owner waits before another claim attempt may take
	// it. Zero (unset) keeps the bridge-derived default; failover
	// reclaim via a higher fencing version is always immediate and
	// independent of this knob. Accepts duration strings ("30s").
	StaleClaimDuration time.Duration `mapstructure:"stale_claim_duration" yaml:"stale_claim_duration" json:"stale_claim_duration"`

	// Retention is the window completed/expired outbox rows (and, for a
	// DLQ store, failed entries) are kept before piggybacked compaction
	// deletes them. Zero (unset) keeps the outbox store default (one hour)
	// and leaves the DLQ store's sweep DISABLED (entries kept forever); a
	// negative value disables outbox compaction (rows kept forever).
	// Deleting a terminal outbox row releases its duplicate-detection
	// identity, so keep this comfortably above any upstream redelivery
	// window.
	Retention time.Duration `mapstructure:"retention" yaml:"retention" json:"retention"`
}

// Kind reports the registry discriminator.
func (SQLiteConfig) Kind() string { return SQLiteKind }

// FreezePluginConfig returns an isolated scalar snapshot for asynchronous build.
func (c SQLiteConfig) FreezePluginConfig() ports.PluginConfig { frozen := c; return &frozen }

// StorageIdentity reports the SQLite database file path — the stable descriptor
// of this store's durable backing, and nothing tunable. The supervisor's
// live-reload guard compares only this, so a tuning-only reload (e.g. changing
// stale_claim_duration or retention) is not mistaken for a backing-store change,
// while repointing Path IS caught even on a hand-built config with no raw
// options. The path is not a secret.
func (c SQLiteConfig) StorageIdentity() string { return c.Path }

// Validate ensures Path is non-empty and durations are sane.
func (c SQLiteConfig) Validate() error {
	if c.Path == "" {
		return errors.New("nativestore: sqlite path is required")
	}
	if c.StaleClaimDuration < 0 {
		return errors.New("nativestore: stale_claim_duration must not be negative")
	}
	return nil
}
