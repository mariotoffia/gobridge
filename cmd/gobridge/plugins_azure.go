//go:build gobridge_azure || gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "azure") }

func registerAzureDecoders(reg *ports.Registry) error {
	return servicebus.Register(reg)
}

func wireAzureFactories(_ context.Context, sup *bridge.Supervisor, logger *slog.Logger, _ ports.MetricsExporter) error {
	factory := servicebus.NewFactory(logger)
	sup.RegisterTransport("servicebus", factory)
	sup.RegisterTransport("azure.servicebus", factory)
	return nil
}
