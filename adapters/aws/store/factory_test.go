package awsstore_test

import (
	"context"
	"testing"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/config"
)

func TestDynamoDBStoreFactory_NewLeaseStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewLeaseStore(context.Background(), config.StoreConfig{Type: "dynamodb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

func TestDynamoDBStoreFactory_NewOutboxStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewOutboxStore(context.Background(), config.StoreConfig{Type: "dynamodb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

func TestDynamoDBStoreFactory_NewDLQStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewDLQStore(context.Background(), config.StoreConfig{Type: "dynamodb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

func TestDynamoDBStoreFactory_WithTableName(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	cfg := config.StoreConfig{
		Type:    "dynamodb",
		Options: map[string]any{"table_name": "custom-table"},
	}

	lease, err := f.NewLeaseStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("lease: unexpected error: %v", err)
	}
	if lease == nil {
		t.Fatal("lease: expected non-nil store")
	}

	outbox, err := f.NewOutboxStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("outbox: unexpected error: %v", err)
	}
	if outbox == nil {
		t.Fatal("outbox: expected non-nil store")
	}

	dlq, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dlq: unexpected error: %v", err)
	}
	if dlq == nil {
		t.Fatal("dlq: expected non-nil store")
	}
}
