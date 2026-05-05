package awsstore

import (
	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	ports.DefaultRegistry.Register(DynamoDBKind, decodeDynamoDBConfig)
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
