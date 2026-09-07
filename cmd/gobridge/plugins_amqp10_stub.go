//go:build !gobridge_amqp10 && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func registerAMQP10Decoders(*ports.Registry) error { return nil }

func wireAMQP10Factories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
	return nil
}
