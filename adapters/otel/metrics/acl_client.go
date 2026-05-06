package otelmetrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// meterClient is the adapter-internal mock seam shielding the
// port-side Exporter from OpenTelemetry SDK types. All
// SDK-typed fields, lazy instrument caches and recording calls
// live behind this interface.
type meterClient interface {
	Counter(name string, value int64, tags []shared.Tag)
	Gauge(name string, value float64, tags []shared.Tag)
	Histogram(name string, value float64, tags []shared.Tag)
	Timer(name string, duration time.Duration, tags []shared.Tag)
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// otelMeterClient is the production meterClient backed by an
// OpenTelemetry MeterProvider plus per-name instrument caches.
type otelMeterClient struct {
	provider     *sdkmetric.MeterProvider
	meter        metric.Meter
	defaultAttrs []attribute.KeyValue

	mu         sync.RWMutex
	counters   map[string]metric.Int64Counter
	gauges     map[string]metric.Float64Gauge
	histograms map[string]metric.Float64Histogram
}

// newMeterClient constructs a production meterClient backed by an
// OTLP HTTP exporter and a periodic-reader MeterProvider.
func newMeterClient(ctx context.Context, cfg Config) (*otelMeterClient, error) {
	exporterOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(cfg.Endpoint),
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlpmetrichttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlpmetrichttp.WithHeaders(cfg.Headers))
	}

	exporter, err := otlpmetrichttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create otlp exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(
				exporter,
				sdkmetric.WithInterval(cfg.FlushInterval),
			),
		),
		sdkmetric.WithResource(res),
	)

	return newMeterClientFromProvider(provider, cfg), nil
}

// newMeterClientFromProvider wraps an existing MeterProvider in a
// meterClient. Used by tests that drive the adapter via a manual
// reader without a network exporter.
func newMeterClientFromProvider(mp *sdkmetric.MeterProvider, cfg Config) *otelMeterClient {
	return &otelMeterClient{
		provider:     mp,
		meter:        mp.Meter("github.com/mariotoffia/gobridge"),
		defaultAttrs: buildDefaultAttrs(cfg.DefaultTags),
		counters:     make(map[string]metric.Int64Counter),
		gauges:       make(map[string]metric.Float64Gauge),
		histograms:   make(map[string]metric.Float64Histogram),
	}
}

func (c *otelMeterClient) Counter(name string, value int64, tags []shared.Tag) {
	counter, err := c.getOrCreateCounter(name)
	if err != nil {
		// Emit failure intentionally dropped: ports.MetricsExporter.Counter
		// has no error return; observability emit failures are
		// non-classified per _design/error-wrapping-policy.adoc §"Observability".
		return
	}
	counter.Add(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Gauge(name string, value float64, tags []shared.Tag) {
	gauge, err := c.getOrCreateGauge(name)
	if err != nil {
		return
	}
	gauge.Record(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Histogram(name string, value float64, tags []shared.Tag) {
	histogram, err := c.getOrCreateHistogram(name)
	if err != nil {
		return
	}
	histogram.Record(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Timer(name string, duration time.Duration, tags []shared.Tag) {
	ms := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	c.Histogram(name, ms, tags)
}

func (c *otelMeterClient) Flush(ctx context.Context) error {
	if err := c.provider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("otel-metrics: flush: %w", err)
	}
	return nil
}

func (c *otelMeterClient) Close(ctx context.Context) error {
	if err := c.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("otel-metrics: shutdown: %w", err)
	}
	return nil
}

func (c *otelMeterClient) getOrCreateCounter(name string) (metric.Int64Counter, error) {
	c.mu.RLock()
	if counter, ok := c.counters[name]; ok {
		c.mu.RUnlock()
		return counter, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if counter, ok := c.counters[name]; ok {
		return counter, nil
	}

	counter, err := c.meter.Int64Counter(name)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create int64 counter: %w", err)
	}
	c.counters[name] = counter
	return counter, nil
}

func (c *otelMeterClient) getOrCreateGauge(name string) (metric.Float64Gauge, error) {
	c.mu.RLock()
	if gauge, ok := c.gauges[name]; ok {
		c.mu.RUnlock()
		return gauge, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if gauge, ok := c.gauges[name]; ok {
		return gauge, nil
	}

	gauge, err := c.meter.Float64Gauge(name)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create float64 gauge: %w", err)
	}
	c.gauges[name] = gauge
	return gauge, nil
}

func (c *otelMeterClient) getOrCreateHistogram(name string) (metric.Float64Histogram, error) {
	c.mu.RLock()
	if histogram, ok := c.histograms[name]; ok {
		c.mu.RUnlock()
		return histogram, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if histogram, ok := c.histograms[name]; ok {
		return histogram, nil
	}

	histogram, err := c.meter.Float64Histogram(name)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create float64 histogram: %w", err)
	}
	c.histograms[name] = histogram
	return histogram, nil
}

func (c *otelMeterClient) buildAttributes(tags []shared.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(c.defaultAttrs)+len(tags))
	attrs = append(attrs, c.defaultAttrs...)
	for _, tag := range tags {
		attrs = append(attrs, attribute.String(tag.Key, tag.Value))
	}
	return attrs
}

func buildDefaultAttrs(tags []shared.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(tags))
	for i, tag := range tags {
		attrs[i] = attribute.String(tag.Key, tag.Value)
	}
	return attrs
}
