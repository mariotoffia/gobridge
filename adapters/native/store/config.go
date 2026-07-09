package nativestore

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contracts.
var (
	_ ports.PluginConfig = (*MemoryConfig)(nil)
	_ ports.PluginConfig = (*SQLiteConfig)(nil)
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
	// See finding c10-memlease-split.
	AcknowledgeSingleReplica bool `mapstructure:"acknowledge_single_replica" yaml:"acknowledge_single_replica" json:"acknowledge_single_replica"`
}

// Kind reports the registry discriminator.
func (MemoryConfig) Kind() string { return MemoryKind }

// Validate is a no-op: the single-replica acknowledgement is a lease-store-only
// concern enforced at construction (MemoryStoreFactory.NewLeaseStore), because
// the same MemoryConfig also configures the outbox and DLQ stores, which have
// no split-brain constraint and must validate without the flag.
func (MemoryConfig) Validate() error { return nil }

// SQLiteConfig is the typed PluginConfig for the SQLite-backed
// outbox/DLQ stores.
type SQLiteConfig struct {
	// Path is the SQLite database file path. ":memory:" selects an
	// in-process database (used by tests).
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
