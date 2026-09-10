//go:build gobridge_amqp10 || gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "amqp10") }

func registerAMQP10Decoders(reg *ports.Registry) error {
	return amqp10.Register(reg)
}

func wireAMQP10Factories(_ context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	factory := amqp10.NewFactory(logger, metrics)
	sup.RegisterTransport("amqp10", factory)
	sup.RegisterTransport("amqp.amqp10", factory)
	return nil
}
