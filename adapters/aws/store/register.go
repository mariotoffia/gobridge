package awsstore

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoders on the
// supplied registry. Composition roots call Register exactly once
// per registry; duplicate registration surfaces ports.ErrDuplicateKind.
func Register(reg *ports.Registry) error {
	return errors.Join(
		reg.Register(DynamoDBKind, decodeDynamoDBConfig),
	)
}

func decodeDynamoDBConfig(raw ports.RawConfig) (ports.PluginConfig, error) {
	var cfg DynamoDBConfig
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
