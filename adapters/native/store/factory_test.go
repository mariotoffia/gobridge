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
	s, err := f.NewLeaseStore(context.Background(), nil)
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
	s, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
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
	s, err := f.NewDLQStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies the SQLite store factory returns an error for the unsupported lease role.
func TestSQLiteStoreFactory_NewLeaseStore_ReturnsError(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for unimplemented SQLite lease store")
	}
	if s != nil {
		t.Fatal("expected nil LeaseStore when error is returned")
	}
}

// Verifies the SQLite factory builds an outbox store from a typed config.
func TestSQLiteStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	s, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the SQLite factory builds a DLQ store from a typed config.
func TestSQLiteStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	s, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies SQLite outbox and DLQ construction fail when the typed config is missing.
func TestSQLiteStoreFactory_MissingPath(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()

	_, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if err == nil {
		t.Fatal("expected error for missing typed config")
	}

	_, err = f.NewDLQStore(context.Background(), &nativestore.SQLiteConfig{})
	if err == nil {
		t.Fatal("expected error for empty Path in typed config")
	}
}
