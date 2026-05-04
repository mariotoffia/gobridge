package awsstore

import (
	"context"
	"fmt"
	"time"

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

// NewLeaseStore creates a DynamoDB-backed lease store from the spec options.
func (f *DynamoDBStoreFactory) NewLeaseStore(_ context.Context, spec ports.StoreSpec) (ports.LeaseStore, error) {
	var opts []dynamodblease.Option
	if name, ok := spec.Options["table_name"].(string); ok {
		opts = append(opts, dynamodblease.WithTableName(name))
	}
	return dynamodblease.NewStore(f.client, opts...), nil
}

// NewOutboxStore creates a DynamoDB-backed outbox store from the spec options.
func (f *DynamoDBStoreFactory) NewOutboxStore(_ context.Context, spec ports.StoreSpec) (ports.OutboxStore, error) {
	var opts []dynamodboutbox.Option
	if name, ok := spec.Options["table_name"].(string); ok {
		opts = append(opts, dynamodboutbox.WithTableName(name))
	}
	if raw, ok := spec.Options["stale_claim_duration"]; ok {
		switch v := raw.(type) {
		case time.Duration:
			opts = append(opts, dynamodboutbox.WithStaleClaimDuration(v))
		case string:
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("dynamodboutbox: invalid stale_claim_duration %q: %w", v, err)
			}
			opts = append(opts, dynamodboutbox.WithStaleClaimDuration(d))
		default:
			return nil, fmt.Errorf("dynamodboutbox: stale_claim_duration must be a duration string or time.Duration, got %T", raw)
		}
	}
	return dynamodboutbox.NewStore(f.client, opts...), nil
}

// NewDLQStore creates a DynamoDB-backed DLQ store from the spec options.
func (f *DynamoDBStoreFactory) NewDLQStore(_ context.Context, spec ports.StoreSpec) (ports.DLQStore, error) {
	var opts []dynamodbdlq.Option
	if name, ok := spec.Options["table_name"].(string); ok {
		opts = append(opts, dynamodbdlq.WithTableName(name))
	}
	return dynamodbdlq.NewStore(f.client, opts...), nil
}
