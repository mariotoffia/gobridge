package awsstore_test

import (
	"context"
	"testing"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/config"
)

// Verifies NewLeaseStore returns a non-nil lease store for dynamodb configuration.
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

// Verifies NewOutboxStore returns a non-nil outbox store for dynamodb configuration.
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

// Verifies NewDLQStore returns a non-nil DLQ store for dynamodb configuration.
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

// Verifies optional table_name in store options is accepted for lease, outbox, and DLQ stores.
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
