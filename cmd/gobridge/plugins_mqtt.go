//go:build gobridge_mqtt || gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "mqtt") }

func registerMQTTDecoders(reg *ports.Registry) error {
	return paho.Register(reg)
}

func wireMQTTFactories(_ context.Context, sup *bridge.Supervisor, logger *slog.Logger, _ ports.MetricsExporter) error {
	sup.RegisterTransport("mqtt", paho.NewFactory(logger))
	return nil
}
