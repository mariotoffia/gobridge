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
// The in-memory store has no user-settable fields; the type exists
// to satisfy the registry contract.
type MemoryConfig struct{}

// Kind reports the registry discriminator.
func (MemoryConfig) Kind() string { return MemoryKind }

// Validate is a no-op for the empty memory config.
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
