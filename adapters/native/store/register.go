package nativestore

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoders on the
// supplied registry. Composition roots call Register exactly once
// per registry; duplicate registration surfaces ports.ErrDuplicateKind.
func Register(reg *ports.Registry) error {
	return errors.Join(
		reg.Register(MemoryKind, decodeMemoryConfig),
		reg.Register(SQLiteKind, decodeSQLiteConfig),
	)
}

func decodeMemoryConfig(_ ports.RawConfig) (ports.PluginConfig, error) {
	return MemoryConfig{}, nil
}

func decodeSQLiteConfig(raw ports.RawConfig) (ports.PluginConfig, error) {
	var cfg SQLiteConfig
	if raw != nil {
		if err := raw.Decode(&cfg); err != nil {
			return nil, err //nolint:wrapcheck // surfaced verbatim by the registry caller.
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
