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
// and runtime tuning options.
func (f *DynamoDBStoreFactory) NewOutboxStore(_ context.Context, cfg ports.PluginConfig, runtime ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodboutbox.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodboutbox.WithTableName(dc.TableName))
	}
	if runtime.StaleClaimDuration > 0 {
		opts = append(opts, dynamodboutbox.WithStaleClaimDuration(runtime.StaleClaimDuration))
	}
	return dynamodboutbox.NewStore(f.client, opts...), nil
}

// NewDLQStore creates a DynamoDB-backed DLQ store from the typed config.
func (f *DynamoDBStoreFactory) NewDLQStore(_ context.Context, cfg ports.PluginConfig) (ports.DLQStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodbdlq.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodbdlq.WithTableName(dc.TableName))
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
