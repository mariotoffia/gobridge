package nativestore_test

import (
	"context"
	"testing"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/config"
)

func TestMemoryStoreFactory_NewLeaseStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), config.StoreConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

func TestMemoryStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewOutboxStore(context.Background(), config.StoreConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

func TestMemoryStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewDLQStore(context.Background(), config.StoreConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

func TestSQLiteStoreFactory_NewLeaseStore_ReturnsNil(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), config.StoreConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil LeaseStore for SQLite factory")
	}
}

func TestSQLiteStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := config.StoreConfig{
		Options: map[string]any{"path": ":memory:"},
	}

	s, err := f.NewOutboxStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

func TestSQLiteStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := config.StoreConfig{
		Options: map[string]any{"path": ":memory:"},
	}

	s, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

func TestSQLiteStoreFactory_MissingPath(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()

	_, err := f.NewOutboxStore(context.Background(), config.StoreConfig{})
	if err == nil {
		t.Fatal("expected error for missing path option")
	}

	_, err = f.NewDLQStore(context.Background(), config.StoreConfig{
		Options: map[string]any{"other": "value"},
	})
	if err == nil {
		t.Fatal("expected error for missing path option")
	}
}
