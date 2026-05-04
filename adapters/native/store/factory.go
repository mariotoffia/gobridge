package nativestore

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory = (*MemoryStoreFactory)(nil)
	_ ports.StoreFactory = (*SQLiteStoreFactory)(nil)
)

// MemoryStoreFactory creates in-memory store instances.
type MemoryStoreFactory struct{}

// NewMemoryStoreFactory creates a MemoryStoreFactory.
func NewMemoryStoreFactory() *MemoryStoreFactory {
	return &MemoryStoreFactory{}
}

// NewLeaseStore creates an in-memory lease store.
func (f *MemoryStoreFactory) NewLeaseStore(_ context.Context, _ ports.StoreSpec) (ports.LeaseStore, error) {
	return memorylease.NewStore(), nil
}

// NewOutboxStore creates an in-memory outbox store.
func (f *MemoryStoreFactory) NewOutboxStore(_ context.Context, _ ports.StoreSpec) (ports.OutboxStore, error) {
	return memoryoutbox.NewStore(), nil
}

// NewDLQStore creates an in-memory DLQ store.
func (f *MemoryStoreFactory) NewDLQStore(_ context.Context, _ ports.StoreSpec) (ports.DLQStore, error) {
	return memorydlq.NewStore(), nil
}

// SQLiteStoreFactory creates SQLite-backed store instances.
// Each New*Store method reads spec.Options["path"] as the database file path.
type SQLiteStoreFactory struct{}

// NewSQLiteStoreFactory creates a SQLiteStoreFactory.
func NewSQLiteStoreFactory() *SQLiteStoreFactory {
	return &SQLiteStoreFactory{}
}

// NewLeaseStore is not supported on SQLite.
func (f *SQLiteStoreFactory) NewLeaseStore(_ context.Context, _ ports.StoreSpec) (ports.LeaseStore, error) {
	return nil, fmt.Errorf("nativestore: SQLite lease store is not implemented; use \"memory\" for single-instance or \"dynamodb\" for clustered deployments")
}

// NewOutboxStore creates a SQLite outbox store from the spec options.
func (f *SQLiteStoreFactory) NewOutboxStore(_ context.Context, spec ports.StoreSpec) (ports.OutboxStore, error) {
	path, err := requiredPath(spec)
	if err != nil {
		return nil, err
	}

	return sqliteoutbox.NewStore(path)
}

// NewDLQStore creates a SQLite DLQ store from the spec options.
func (f *SQLiteStoreFactory) NewDLQStore(_ context.Context, spec ports.StoreSpec) (ports.DLQStore, error) {
	path, err := requiredPath(spec)
	if err != nil {
		return nil, err
	}

	return sqlitedlq.NewStore(path)
}

func requiredPath(spec ports.StoreSpec) (string, error) {
	if spec.Options == nil {
		return "", fmt.Errorf("nativestore: missing required option \"path\" in store spec (options is nil)")
	}
	v, ok := spec.Options["path"]
	if !ok {
		return "", fmt.Errorf("nativestore: missing required option \"path\" in store spec")
	}

	path, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("nativestore: option \"path\" must be a string, got %T", v)
	}

	return path, nil
}
