package dynamodbmanagedsubscriptions

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
)

var errDeadlineProbe = errors.New("deadline probe")

type deadlineProbeDynamo struct {
	deadlineSeen bool
}

func (d *deadlineProbeDynamo) record(ctx context.Context) error {
	_, d.deadlineSeen = ctx.Deadline()
	return errDeadlineProbe
}

func (d *deadlineProbeDynamo) GetItem(ctx context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, d.record(ctx)
}

func (d *deadlineProbeDynamo) UpdateItem(ctx context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return nil, d.record(ctx)
}

func (d *deadlineProbeDynamo) CreateTable(ctx context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return nil, d.record(ctx)
}

func (d *deadlineProbeDynamo) DescribeTable(ctx context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, d.record(ctx)
}

func TestStore_DynamoDBCallsHaveAdapterOwnedDeadline(t *testing.T) {
	tests := map[string]func(*Store) error{
		"ensure table": func(store *Store) error {
			return store.EnsureTable(context.Background())
		},
		"preflight": func(store *Store) error {
			return store.Preflight(context.Background())
		},
		"list": func(store *Store) error {
			_, err := store.List(context.Background(), "identity")
			return err
		},
		"remember": func(store *Store) error {
			return store.Remember(context.Background(), "identity", []string{"events/#"})
		},
		"forget": func(store *Store) error {
			return store.Forget(context.Background(), "identity", []string{"events/#"})
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			client := &deadlineProbeDynamo{}
			store := &Store{client: client, tableName: DefaultTableName}

			_ = call(store)

			assert.True(t, client.deadlineSeen)
		})
	}
}
