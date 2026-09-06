//go:build !gobridge_otel && !gobridge_all

package main

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

func newMetricsExporter(context.Context, *slog.Logger) (ports.MetricsExporter, func(context.Context) error, error) {
	return nil, nil, nil
}

func newTracer(context.Context, *slog.Logger) (ports.Tracer, func(context.Context) error, error) {
	return nil, nil, nil
}
