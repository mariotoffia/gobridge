package awsstore

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var (
	_ ports.PluginConfig    = (*DynamoDBConfig)(nil)
	_ ports.FreezableConfig = (*DynamoDBConfig)(nil)
)

// DynamoDBKind is the registry discriminator for DynamoDB-backed
// lease, outbox, DLQ, and managed-subscription stores.
const DynamoDBKind = "dynamodb"

const (
	// DefaultDynamoDBLeaseTableName is the authoritative lease-table default.
	DefaultDynamoDBLeaseTableName = dynamodblease.DefaultTableName
	// DefaultDynamoDBOutboxTableName is the authoritative outbox-table default.
	DefaultDynamoDBOutboxTableName = dynamodboutbox.DefaultTableName
	// DefaultDynamoDBDLQTableName is the authoritative DLQ-table default.
	DefaultDynamoDBDLQTableName = dynamodbdlq.DefaultTableName
	// DefaultDynamoDBManagedSubscriptionsTableName is the authoritative exact-filter history-table default.
	DefaultDynamoDBManagedSubscriptionsTableName = dynamodbmanagedsubscriptions.DefaultTableName
)

// ResolveDynamoDBTableName resolves the exact table used by a store role before
// runtime preflight or deployment IAM grants. Explicit names always win.
func ResolveDynamoDBTableName(role, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	switch strings.ToLower(role) {
	case "lease":
		return DefaultDynamoDBLeaseTableName, nil
	case "outbox":
		return DefaultDynamoDBOutboxTableName, nil
	case "dlq":
		return DefaultDynamoDBDLQTableName, nil
	case "managed_subscriptions":
		return DefaultDynamoDBManagedSubscriptionsTableName, nil
	default:
		return "", fmt.Errorf("awsstore: unknown DynamoDB store role %q", role)
	}
}

// DynamoDBConfig is the typed PluginConfig for DynamoDB-backed
// stores. It is shared across lease, outbox, DLQ, and managed-subscription
// roles. The same table-name knob applies to all four roles; role-specific
// knobs are ignored by the other roles.
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

// FreezePluginConfig returns an isolated scalar snapshot for asynchronous build.
func (c DynamoDBConfig) FreezePluginConfig() ports.PluginConfig { frozen := c; return &frozen }

// StorageIdentity reports the DynamoDB table name — the stable descriptor of
// this store's durable backing, and nothing tunable. The supervisor's
// live-reload guard compares only this, so a tuning-only reload (e.g. changing
// stale_claim_duration or max_scan_pages) is not mistaken for a backing-store
// change, while repointing TableName IS caught even on a hand-built config with
// no raw options. An empty TableName means "the store's built-in default table",
// so two empty configs share one identity — correct, since they resolve to the
// same table. The table name is not a secret.
func (c DynamoDBConfig) StorageIdentity() string { return c.TableName }

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
