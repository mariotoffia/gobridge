//go:build gobridge_http || gobridge_all

package main

import (
	"context"
	"log/slog"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "http") }

func registerHTTPDecoders(reg *ports.Registry) error {
	return httptransport.Register(reg)
}

func wireHTTPFactories(_ context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	opts := []httptransport.FactoryOption{httptransport.WithFactoryLogger(logger)}
	if metrics != nil {
		opts = append(opts, httptransport.WithFactoryMetrics(metrics))
	}
	sup.RegisterTransport("http", httptransport.NewFactory(opts...))
	return nil
}
