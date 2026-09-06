//go:build !gobridge_native && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func registerNativeDecoders(*ports.Registry) error { return nil }

func wireNativeFactories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
	return nil
}

func seedNativeStores(context.Context, *bridge.Builder) error { return nil }
