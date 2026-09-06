package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// compiledFamilies lists the plugin families linked into this binary. A
// blank root has none. Family files append to it from init().
//
//nolint:gochecknoglobals // Build-tag metadata is assembled at initialization and then read-only.
var compiledFamilies []string

func registerAllDecoders(reg *ports.Registry) error {
	return errors.Join(
		registerMQTTDecoders(reg),
		registerNativeDecoders(reg),
	)
}

func wireAllFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	return errors.Join(
		wireMQTTFactories(ctx, sup, logger, metrics),
		wireNativeFactories(ctx, sup, logger, metrics),
	)
}

// seedAllStores supplies the same stores to the one-shot Builder as the Supervisor.
func seedAllStores(ctx context.Context, b *bridge.Builder) error {
	return seedNativeStores(ctx, b)
}
