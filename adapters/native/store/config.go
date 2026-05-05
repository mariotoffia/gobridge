package nativestore

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contracts.
var (
	_ ports.PluginConfig = (*MemoryConfig)(nil)
	_ ports.PluginConfig = (*SQLiteConfig)(nil)
)

// MemoryKind is the registry discriminator for the in-memory store.
const MemoryKind = "memory"

// SQLiteKind is the registry discriminator for the SQLite store.
const SQLiteKind = "sqlite"

// MemoryConfig is the typed PluginConfig for the in-memory store.
// The in-memory store has no user-settable fields; the type exists
// to satisfy the registry contract.
type MemoryConfig struct{}

// Kind reports the registry discriminator.
func (MemoryConfig) Kind() string { return MemoryKind }

// Validate is a no-op for the empty memory config.
func (MemoryConfig) Validate() error { return nil }

// SQLiteConfig is the typed PluginConfig for the SQLite-backed
// outbox/DLQ stores.
type SQLiteConfig struct {
	// Path is the SQLite database file path. ":memory:" selects an
	// in-process database (used by tests).
	Path string `mapstructure:"path" yaml:"path" json:"path"`
}

// Kind reports the registry discriminator.
func (SQLiteConfig) Kind() string { return SQLiteKind }

// Validate ensures Path is non-empty.
func (c SQLiteConfig) Validate() error {
	if c.Path == "" {
		return errors.New("nativestore: sqlite path is required")
	}
	return nil
}
