//go:build !gobridge_aws && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func registerAWSDecoders(*ports.Registry) error { return nil }

func wireAWSFactories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
	return nil
}

func seedAWSStores(context.Context, *bridge.Builder) error { return nil }
