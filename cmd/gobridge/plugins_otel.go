//go:build gobridge_otel || gobridge_all

package main

import (
	"context"
	"log/slog"

	otelmetrics "github.com/mariotoffia/gobridge/adapters/otel/metrics"
	oteltracing "github.com/mariotoffia/gobridge/adapters/otel/tracing"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; exporters are constructed explicitly.
func init() { compiledFamilies = append(compiledFamilies, "otel") }

func newMetricsExporter(ctx context.Context, _ *slog.Logger) (ports.MetricsExporter, func(context.Context) error, error) {
	exporter, err := otelmetrics.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	return exporter, exporter.Close, nil
}

func newTracer(ctx context.Context, _ *slog.Logger) (ports.Tracer, func(context.Context) error, error) {
	tracer, err := oteltracing.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	return tracer, tracer.Close, nil
}
