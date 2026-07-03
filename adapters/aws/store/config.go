package awsstore

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.PluginConfig = (*DynamoDBConfig)(nil)

// DynamoDBKind is the registry discriminator for DynamoDB-backed
// lease, outbox, and DLQ stores.
const DynamoDBKind = "dynamodb"

// DynamoDBConfig is the typed PluginConfig for DynamoDB-backed
// stores. It is shared across lease/outbox/DLQ since the same
// table-name knob applies to all three roles; role-specific knobs
// are ignored by the other roles.
type DynamoDBConfig struct {
	// TableName overrides the default DynamoDB table name. When
	// empty, the underlying store uses its built-in default.
	TableName string `mapstructure:"table_name" yaml:"table_name" json:"table_name"`

	// StaleClaimDuration (outbox only) overrides the runtime-derived
	// stale-claim reclaim window: how long a claim stranded by a
	// crashed same-owner waits before another claim attempt may take
	// it. Zero (unset) keeps the bridge-derived default; failover
	// reclaim via a higher fencing version is always immediate and
	// independent of this knob. Accepts duration strings ("30s").
	StaleClaimDuration time.Duration `mapstructure:"stale_claim_duration" yaml:"stale_claim_duration" json:"stale_claim_duration"`

	// CompactionGrace (outbox only) is the window completed/expired
	// outbox items are kept before DynamoDB item TTL deletes them.
	// Zero (unset) keeps the store default. Deleting a terminal item
	// releases its duplicate-detection identity, so keep this
	// comfortably above any upstream redelivery window.
	CompactionGrace time.Duration `mapstructure:"compaction_grace" yaml:"compaction_grace" json:"compaction_grace"`

	// Retention (DLQ only) sets a TTL on dead-letter entries
	// (ttl = failed_at + retention) so investigated-and-forgotten
	// failures eventually clean up. Zero (unset) keeps entries until
	// explicitly deleted. Use a days-scale value in production so
	// investigators have time to inspect dead-lettered messages.
	Retention time.Duration `mapstructure:"retention" yaml:"retention" json:"retention"`

	// MaxScanPages (DLQ only) bounds the number of DynamoDB pages an
	// index-less List or a Purge reads before stopping, preventing an
	// unbounded full-table scan on large DLQ tables. Zero (unset)
	// keeps the store default (100); a negative value disables the
	// bound.
	MaxScanPages int `mapstructure:"max_scan_pages" yaml:"max_scan_pages" json:"max_scan_pages"`
}

// Kind reports the registry discriminator.
func (DynamoDBConfig) Kind() string { return DynamoDBKind }

// Validate rejects nonsensical duration values. TableName is optional.
func (c DynamoDBConfig) Validate() error {
	if c.StaleClaimDuration < 0 {
		return errors.New("awsstore: stale_claim_duration must not be negative")
	}
	if c.CompactionGrace < 0 {
		return errors.New("awsstore: compaction_grace must not be negative")
	}
	if c.Retention < 0 {
		return errors.New("awsstore: retention must not be negative")
	}
	return nil
}
