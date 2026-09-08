package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type configSource struct {
	layer        config.Layer
	store        ports.ConfigStore
	singleWriter bool
}

// newConfigSource composes one base layer and its admin persistence boundary.
// Start validates the bootstrap config before calling it. Only local development
// may provision a config table; production requires an existing table.
func (a *App) newConfigSource(ctx context.Context) (configSource, error) {
	var src configSource
	switch a.cfg.ConfigSource {
	case deployinfra.ConfigSourceFile:
		src = configSource{
			layer: config.Layer{
				Name:    "file",
				Loader:  fileconfig.NewSource(a.cfg.ConfigFilePath, a.pluginRegistry),
				Watcher: newPollWatcher(ctx, a.cfg, a.pluginRegistry, a.logger),
			},
			store:        &cfgparser.FileStore{Path: a.cfg.ConfigFilePath, Registry: a.pluginRegistry},
			singleWriter: a.cfg.NodeRole == deployinfra.NodeRoleControl,
		}
	case deployinfra.ConfigSourceDynamoDB:
		if err := a.ensureDynamoDBClient(ctx); err != nil {
			return configSource{}, err
		}
		settings := a.cfg.ConfigDynamoDB
		opts := []ddbconfig.Option{
			ddbconfig.WithTableName(settings.TableName),
			ddbconfig.WithBridgeID(a.cfg.BridgeID),
			ddbconfig.WithRegistry(a.pluginRegistry),
			ddbconfig.WithPollInterval(a.cfg.EffectivePollInterval()),
			ddbconfig.WithLogger(a.logger),
			ddbconfig.WithClock(a.clk),
		}
		if settings.WatchMode == "streams" {
			opts = append(opts, ddbconfig.WithWatchMode(ddbconfig.ModeStreams),
				ddbconfig.WithStreamsClient(newDynamoDBStreamsClient(a.dynamoDBClient)))
		}
		if settings.StreamPollInterval != "" {
			interval, err := time.ParseDuration(settings.StreamPollInterval)
			if err != nil {
				return configSource{}, fmt.Errorf("bootstrap: config stream poll interval: %w", err)
			}
			opts = append(opts, ddbconfig.WithStreamPollInterval(interval))
		}
		loader := ddbconfig.NewLoader(a.dynamoDBClient, opts...)
		if a.cfg.DevMode {
			if err := loader.EnsureTable(ctx); err != nil {
				return configSource{}, fmt.Errorf("bootstrap: ensure config table: %w", err)
			}
		}
		src = configSource{
			layer: config.Layer{Name: "dynamodb", Loader: loader, Watcher: loader},
			store: loader,
			// ConditionalConfigStore enforces CAS, not a single-writer assumption.
			singleWriter: false,
		}
	default:
		return configSource{}, fmt.Errorf("bootstrap: unsupported config source %q", a.cfg.ConfigSource)
	}
	// Wrap only the load boundary. The watcher and admin store retain their
	// original identity, capabilities and missing-config behavior.
	src.layer.Loader = &startEmptySource{
		loader: src.layer.Loader, logger: a.logger,
		fallback: func() *ports.BridgeConfig { return defaultLogicalConfig(a.cfg) },
	}
	return src, nil
}

type startEmptySource struct {
	loader   ports.Loader
	fallback func() *ports.BridgeConfig
	logger   *slog.Logger
}

func (s *startEmptySource) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	cfg, err := s.loader.Load(ctx)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, shared.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		if s.logger != nil {
			s.logger.Warn("bootstrap: config not found; falling back to empty default config " +
				"(no routes will be bridged) — verify the selected config source is seeded")
		}
		return s.fallback(), nil
	}
	return nil, err
}

var _ ports.Loader = (*startEmptySource)(nil)
