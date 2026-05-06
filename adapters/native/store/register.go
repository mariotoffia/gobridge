package nativestore

import (
	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	ports.DefaultRegistry.Register(MemoryKind, decodeMemoryConfig)
	ports.DefaultRegistry.Register(SQLiteKind, decodeSQLiteConfig)
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
