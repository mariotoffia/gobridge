package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// compiledFamilies lists the plugin families linked into this binary. A
// blank root has none. Family files append to it from init().
//
//nolint:gochecknoglobals // Build-tag metadata is assembled at initialization and then read-only.
var compiledFamilies []string

func pluginSummary() string {
	families := slices.Clone(compiledFamilies)
	slices.Sort(families)
	return fmt.Sprintf("families=%v", families)
}

func versionLine() string {
	return fmt.Sprintf("gobridge %s (%s) %s", orDefault(version, "dev"), orDefault(gitSHA, "dev"), pluginSummary())
}

func registerAllDecoders(reg *ports.Registry) error {
	return errors.Join(
		registerMQTTDecoders(reg),
		registerNativeDecoders(reg),
		registerAWSDecoders(reg),
	)
}

func wireAllFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	return errors.Join(
		wireMQTTFactories(ctx, sup, logger, metrics),
		wireNativeFactories(ctx, sup, logger, metrics),
		wireAWSFactories(ctx, sup, logger, metrics),
	)
}

// seedAllStores supplies the same stores to the one-shot Builder as the Supervisor.
func seedAllStores(ctx context.Context, b *bridge.Builder) error {
	return errors.Join(
		seedNativeStores(ctx, b),
		seedAWSStores(ctx, b),
	)
}
