package nativestore_test

import (
	"context"
	"testing"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies the memory store factory returns a non-nil lease store.
func TestMemoryStoreFactory_NewLeaseStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), ports.StoreSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

// Verifies the memory store factory returns a non-nil outbox store.
func TestMemoryStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewOutboxStore(context.Background(), ports.StoreSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the memory store factory returns a non-nil DLQ store.
func TestMemoryStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewDLQStore(context.Background(), ports.StoreSpec{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies the SQLite store factory returns a nil lease store.
func TestSQLiteStoreFactory_NewLeaseStore_ReturnsError(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), ports.StoreSpec{})
	if err == nil {
		t.Fatal("expected error for unimplemented SQLite lease store")
	}
	if s != nil {
		t.Fatal("expected nil LeaseStore when error is returned")
	}
}

// Verifies the SQLite factory builds an outbox store when a database path is configured.
func TestSQLiteStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	spec := ports.StoreSpec{
		Options: map[string]any{"path": ":memory:"},
	}

	s, err := f.NewOutboxStore(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the SQLite factory builds a DLQ store when a database path is configured.
func TestSQLiteStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	spec := ports.StoreSpec{
		Options: map[string]any{"path": ":memory:"},
	}

	s, err := f.NewDLQStore(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies SQLite outbox and DLQ construction fail when the path option is missing.
func TestSQLiteStoreFactory_MissingPath(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()

	_, err := f.NewOutboxStore(context.Background(), ports.StoreSpec{})
	if err == nil {
		t.Fatal("expected error for missing path option")
	}

	_, err = f.NewDLQStore(context.Background(), ports.StoreSpec{
		Options: map[string]any{"other": "value"},
	})
	if err == nil {
		t.Fatal("expected error for missing path option")
	}
}
