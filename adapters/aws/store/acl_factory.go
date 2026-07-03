package awsstore

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory            = (*DynamoDBStoreFactory)(nil)
	_ ports.DistributedStoreFactory = (*DynamoDBStoreFactory)(nil)
)

// DynamoDBStoreFactory creates DynamoDB-backed lease, outbox, and DLQ stores.
type DynamoDBStoreFactory struct {
	client *dynamodb.Client
}

// NewDynamoDBStoreFactory returns a factory that creates DynamoDB stores
// using the provided client.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL
// factory exists to wrap; it is injected by the composition root and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewDynamoDBStoreFactory(client *dynamodb.Client) *DynamoDBStoreFactory {
	return &DynamoDBStoreFactory{client: client}
}

// IsDistributed marks DynamoDB stores as cross-process coordination capable.
func (f *DynamoDBStoreFactory) IsDistributed() bool { return true }

// NewLeaseStore creates a DynamoDB-backed lease store from the typed config.
func (f *DynamoDBStoreFactory) NewLeaseStore(_ context.Context, cfg ports.PluginConfig) (ports.LeaseStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodblease.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodblease.WithTableName(dc.TableName))
	}
	return dynamodblease.NewStore(f.client, opts...), nil
}

// NewOutboxStore creates a DynamoDB-backed outbox store from the typed config
// and runtime tuning options. The typed config's stale_claim_duration (when
// > 0) overrides the runtime-derived value; compaction_grace (when > 0)
// overrides the store's default item-TTL grace.
func (f *DynamoDBStoreFactory) NewOutboxStore(_ context.Context, cfg ports.PluginConfig, runtime ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodboutbox.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodboutbox.WithTableName(dc.TableName))
	}
	staleClaim := runtime.StaleClaimDuration
	if dc.StaleClaimDuration > 0 {
		staleClaim = dc.StaleClaimDuration
	}
	if staleClaim > 0 {
		opts = append(opts, dynamodboutbox.WithStaleClaimDuration(staleClaim))
	}
	if dc.CompactionGrace > 0 {
		opts = append(opts, dynamodboutbox.WithCompactionGrace(dc.CompactionGrace))
	}
	// Thread the runtime exporter so store-level counters (e.g.
	// shared.MetricOutboxClaimConflicts) are actually emitted in
	// config-driven deployments. A nil exporter leaves the store's default
	// no-op meter in place (WithMetrics ignores nil), so this never installs
	// a dereferenceable-nil exporter.
	if runtime.Metrics != nil {
		opts = append(opts, dynamodboutbox.WithMetrics(runtime.Metrics))
	}
	return dynamodboutbox.NewStore(f.client, opts...), nil
}

// NewDLQStore creates a DynamoDB-backed DLQ store from the typed config.
// A positive retention enables item TTL on dead-letter entries; a
// non-zero max_scan_pages overrides the default scan bound (negative
// disables the bound).
func (f *DynamoDBStoreFactory) NewDLQStore(_ context.Context, cfg ports.PluginConfig) (ports.DLQStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodbdlq.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodbdlq.WithTableName(dc.TableName))
	}
	if dc.Retention > 0 {
		opts = append(opts, dynamodbdlq.WithRetention(dc.Retention))
	}
	if dc.MaxScanPages != 0 {
		opts = append(opts, dynamodbdlq.WithMaxScanPages(max(dc.MaxScanPages, 0)))
	}
	return dynamodbdlq.NewStore(f.client, opts...), nil
}

// dynamoDBConfigFromOrZero accepts a *DynamoDBConfig, DynamoDBConfig,
// or nil. Other concrete types are an error: an unexpected
// PluginConfig is a programming error in the composition root.
func dynamoDBConfigFromOrZero(cfg ports.PluginConfig) (DynamoDBConfig, error) {
	switch v := cfg.(type) {
	case nil:
		return DynamoDBConfig{}, nil
	case *DynamoDBConfig:
		if v == nil {
			return DynamoDBConfig{}, nil
		}
		return *v, nil
	case DynamoDBConfig:
		return v, nil
	default:
		return DynamoDBConfig{}, fmt.Errorf("awsstore: DynamoDB store requires a *DynamoDBConfig, got %T", cfg)
	}
}
