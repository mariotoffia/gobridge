//go:build !gobridge_mqtt && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func registerMQTTDecoders(*ports.Registry) error { return nil }

func wireMQTTFactories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
	return nil
}
