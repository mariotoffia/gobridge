//go:build gobridge_amqp091 || gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "amqp091") }

func registerAMQP091Decoders(reg *ports.Registry) error {
	return amqp091.Register(reg)
}

func wireAMQP091Factories(_ context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	factory := amqp091.NewFactory(logger, metrics)
	sup.RegisterTransport("amqp091", factory)
	sup.RegisterTransport("amqp.amqp091", factory)
	return nil
}
