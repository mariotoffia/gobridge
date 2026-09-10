//go:build !gobridge_http && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func registerHTTPDecoders(*ports.Registry) error { return nil }

func wireHTTPFactories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
	return nil
}
