package bootstrap

import (
	"context"
	"fmt"
	"time"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
)

type configSource struct {
	layer        config.Layer
	store        ports.ConfigStore
	singleWriter bool
}

// newConfigSource composes one base layer and its admin persistence boundary.
// Start validates the bootstrap config before calling it. Only local development
// may provision a config table later, behind the control plane; this function
// performs no repository reads. Production requires an existing table.
func (a *App) newConfigSource(ctx context.Context) (configSource, error) {
	var src configSource
	switch a.cfg.ConfigSource {
	case deployinfra.ConfigSourceFile:
		src = configSource{
			layer: config.Layer{
				Name:   "file",
				Loader: fileconfig.NewSource(a.cfg.ConfigFilePath, a.pluginRegistry),
				Watcher: fileconfig.NewWatcher(a.cfg.ConfigFilePath, a.pluginRegistry,
					fileconfig.WithMode(fileconfig.ModePoll), fileconfig.WithPollInterval(a.cfg.EffectivePollInterval()), fileconfig.WithClock(a.clk)),
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

		src = configSource{
			layer: config.Layer{Name: "dynamodb", Loader: loader, Watcher: loader},
			store: loader,
			// ConditionalConfigStore enforces CAS, not a single-writer assumption.
			singleWriter: false,
		}
	default:
		return configSource{}, fmt.Errorf("bootstrap: unsupported config source %q", a.cfg.ConfigSource)
	}
	return src, nil
}
