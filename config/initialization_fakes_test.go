package config

import (
	"context"
	"maps"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type initialStore struct {
	cfg       *ports.BridgeConfig
	loadErr   error
	writeErr  error
	winner    *ports.BridgeConfig
	persisted *ports.BridgeConfig
}

func (s *initialStore) Load(context.Context) (*ports.BridgeConfig, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.cfg == nil {
		return nil, shared.ErrNotFound
	}
	return s.cfg, nil
}

func (*initialStore) Save(context.Context, *ports.BridgeConfig) error {
	panic("initialization must never use Save")
}

func (*initialStore) Validate(_ context.Context, cfg *ports.BridgeConfig) ([]string, error) {
	return ValidateWithWarnings(cfg)
}

func (*initialStore) Merge(_ context.Context, a, b *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	return DefaultMerge(a, b)
}

func (s *initialStore) CreateIfAbsent(_ context.Context, cfg *ports.BridgeConfig) (bool, error) {
	if s.winner != nil {
		s.cfg = s.winner
		return false, s.writeErr
	}
	if s.writeErr == nil {
		s.cfg, s.persisted = cfg, cfg
		cfg.Version = 1
	}
	return s.writeErr == nil, s.writeErr
}

type initialPlugin struct{ Values map[string]string }

func (*initialPlugin) Kind() string    { return "mqtt" }
func (*initialPlugin) Validate() error { return nil }
func (p *initialPlugin) FreezePluginConfig() ports.PluginConfig {
	return &initialPlugin{Values: maps.Clone(p.Values)}
}

var (
	_ ports.ConfigStore       = (*initialStore)(nil)
	_ ports.ConfigInitializer = (*initialStore)(nil)
	_ ports.FreezableConfig   = (*initialPlugin)(nil)
)
