package nativestore

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ bridge.StoreFactory = (*MemoryStoreFactory)(nil)
	_ bridge.StoreFactory = (*SQLiteStoreFactory)(nil)
)

// MemoryStoreFactory creates in-memory store instances.
type MemoryStoreFactory struct{}

func NewMemoryStoreFactory() *MemoryStoreFactory {
	return &MemoryStoreFactory{}
}

func (f *MemoryStoreFactory) NewLeaseStore(_ context.Context, _ config.StoreConfig) (ports.LeaseStore, error) {
	return memorylease.NewStore(), nil
}

func (f *MemoryStoreFactory) NewOutboxStore(_ context.Context, _ config.StoreConfig) (ports.OutboxStore, error) {
	return memoryoutbox.NewStore(), nil
}

func (f *MemoryStoreFactory) NewDLQStore(_ context.Context, _ config.StoreConfig) (ports.DLQStore, error) {
	return memorydlq.NewStore(), nil
}

// SQLiteStoreFactory creates SQLite-backed store instances.
// Each New*Store method reads cfg.Options["path"] as the database file path.
type SQLiteStoreFactory struct{}

func NewSQLiteStoreFactory() *SQLiteStoreFactory {
	return &SQLiteStoreFactory{}
}

func (f *SQLiteStoreFactory) NewLeaseStore(_ context.Context, _ config.StoreConfig) (ports.LeaseStore, error) {
	return nil, fmt.Errorf("nativestore: SQLite lease store is not implemented; use \"memory\" for single-instance or \"dynamodb\" for clustered deployments")
}

func (f *SQLiteStoreFactory) NewOutboxStore(_ context.Context, cfg config.StoreConfig) (ports.OutboxStore, error) {
	path, err := requiredPath(cfg)
	if err != nil {
		return nil, err
	}

	return sqliteoutbox.NewStore(path)
}

func (f *SQLiteStoreFactory) NewDLQStore(_ context.Context, cfg config.StoreConfig) (ports.DLQStore, error) {
	path, err := requiredPath(cfg)
	if err != nil {
		return nil, err
	}

	return sqlitedlq.NewStore(path)
}

func requiredPath(cfg config.StoreConfig) (string, error) {
	if cfg.Options == nil {
		return "", fmt.Errorf("nativestore: missing required option \"path\" in store config (options is nil)")
	}
	v, ok := cfg.Options["path"]
	if !ok {
		return "", fmt.Errorf("nativestore: missing required option \"path\" in store config")
	}

	path, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("nativestore: option \"path\" must be a string, got %T", v)
	}

	return path, nil
}
