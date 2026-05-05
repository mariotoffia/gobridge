package awsstore_test

import (
	"context"
	"testing"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies NewLeaseStore returns a non-nil lease store for nil config.
func TestDynamoDBStoreFactory_NewLeaseStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewLeaseStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

// Verifies NewOutboxStore returns a non-nil outbox store for nil config.
func TestDynamoDBStoreFactory_NewOutboxStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies NewDLQStore returns a non-nil DLQ store for nil config.
func TestDynamoDBStoreFactory_NewDLQStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewDLQStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies optional table_name in the typed config is accepted for lease, outbox, and DLQ stores.
func TestDynamoDBStoreFactory_WithTableName(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	cfg := &awsstore.DynamoDBConfig{TableName: "custom-table"}

	lease, err := f.NewLeaseStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("lease: unexpected error: %v", err)
	}
	if lease == nil {
		t.Fatal("lease: expected non-nil store")
	}

	outbox, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{})
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
