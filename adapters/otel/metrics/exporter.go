package otelmetrics

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.MetricsExporter = (*Exporter)(nil)

// Exporter implements [ports.MetricsExporter] for OpenTelemetry. The
// SDK boundary is encapsulated in the unexported meterClient seam
// declared in acl_client.go; this file is SDK-import-free.
type Exporter struct {
	config Config
	client meterClient
}

// New creates a new OTEL metrics exporter backed by an OTLP HTTP
// exporter. The returned Exporter is safe for concurrent use.
func New(ctx context.Context, opts ...Option) (*Exporter, error) {
	e := &Exporter{}

	for _, opt := range opts {
		opt(e)
	}
	applyDefaults(&e.config)

	client, err := newMeterClient(ctx, e.config)
	if err != nil {
		return nil, err
	}
	e.client = client

	return e, nil
}

// Counter increments a counter metric.
func (e *Exporter) Counter(name string, value int64, tags ...shared.Tag) {
	if e.client == nil {
		return
	}
	e.client.Counter(name, value, tags)
}

// Gauge sets a gauge metric value.
func (e *Exporter) Gauge(name string, value float64, tags ...shared.Tag) {
	if e.client == nil {
		return
	}
	e.client.Gauge(name, value, tags)
}

// Histogram records a histogram value.
func (e *Exporter) Histogram(name string, value float64, tags ...shared.Tag) {
	if e.client == nil {
		return
	}
	e.client.Histogram(name, value, tags)
}

// Timer records a duration as milliseconds into a histogram.
func (e *Exporter) Timer(name string, duration time.Duration, tags ...shared.Tag) {
	if e.client == nil {
		return
	}
	e.client.Timer(name, duration, tags)
}

// Flush forces a metric export.
func (e *Exporter) Flush(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	return e.client.Flush(ctx)
}

// Close shuts down the exporter.
func (e *Exporter) Close(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	return e.client.Close(ctx)
}
