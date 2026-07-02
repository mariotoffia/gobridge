package otelmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// errInstrumentLimit is returned when the instrument cache is full and
// a new (likely dynamic) metric name is rejected (K9).
var errInstrumentLimit = errors.New("instrument cache limit reached")

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

	maxInstruments int
	onError        func(error)

	mu         sync.RWMutex
	counters   map[string]metric.Int64Counter
	gauges     map[string]metric.Float64Gauge
	histograms map[string]metric.Float64Histogram
}

// newMeterClient constructs a production meterClient backed by an
// OTLP HTTP exporter and a periodic-reader MeterProvider.
func newMeterClient(ctx context.Context, cfg Config) (*otelMeterClient, error) {
	var exporterOpts []otlpmetrichttp.Option
	// Only pin the endpoint when explicitly configured; otherwise the
	// SDK honors OTEL_EXPORTER_OTLP[_METRICS]_ENDPOINT env vars (K7).
	if cfg.Endpoint != "" {
		exporterOpts = append(exporterOpts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
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

	// resource.Default() already merges OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES; only override attributes explicitly set
	// via options so env-provided values are not clobbered (K7).
	var resAttrs []attribute.KeyValue
	if cfg.ServiceName != "" {
		resAttrs = append(resAttrs, semconv.ServiceName(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironment(cfg.Environment))
	}

	// NewSchemaless avoids a schema-URL conflict with resource.Default(),
	// whose semconv version may differ from ours across SDK bumps.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("otel-metrics: create resource: %w", err)
	}

	observed := &observedMetricExporter{Exporter: exporter, onError: cfg.errorHandler}

	readerOpts := []sdkmetric.PeriodicReaderOption{
		sdkmetric.WithInterval(cfg.FlushInterval),
	}
	if cfg.ExportTimeout > 0 {
		readerOpts = append(readerOpts, sdkmetric.WithTimeout(cfg.ExportTimeout))
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(observed, readerOpts...)),
		sdkmetric.WithResource(res),
	)

	return newMeterClientFromProvider(provider, cfg), nil
}

// newMeterClientFromProvider wraps an existing MeterProvider in a
// meterClient. Used by tests that drive the adapter via a manual
// reader without a network exporter.
func newMeterClientFromProvider(mp *sdkmetric.MeterProvider, cfg Config) *otelMeterClient {
	maxInstruments := cfg.MaxInstruments
	if maxInstruments == 0 {
		maxInstruments = defaultMaxInstruments
	}
	return &otelMeterClient{
		provider:       mp,
		meter:          mp.Meter("github.com/mariotoffia/gobridge"),
		defaultAttrs:   buildDefaultAttrs(cfg.DefaultTags),
		maxInstruments: maxInstruments,
		onError:        cfg.errorHandler,
		counters:       make(map[string]metric.Int64Counter),
		gauges:         make(map[string]metric.Float64Gauge),
		histograms:     make(map[string]metric.Float64Histogram),
	}
}

// reportError surfaces a non-fatal emit/export failure through the
// configured error handler. The port methods have no error return, so
// without a handler these failures are lost; the handler is the
// classified visibility path (K3).
func (c *otelMeterClient) reportError(err error) {
	if err != nil && c.onError != nil {
		c.onError(err)
	}
}

func (c *otelMeterClient) Counter(name string, value int64, tags []shared.Tag) {
	counter, err := c.getOrCreateCounter(name)
	if err != nil {
		c.reportError(err)
		return
	}
	counter.Add(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Gauge(name string, value float64, tags []shared.Tag) {
	gauge, err := c.getOrCreateGauge(name)
	if err != nil {
		c.reportError(err)
		return
	}
	gauge.Record(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Histogram(name string, value float64, tags []shared.Tag) {
	histogram, err := c.getOrCreateHistogram(name)
	if err != nil {
		c.reportError(err)
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

// instrumentCountLocked returns the total distinct instrument count.
// Callers must hold c.mu (read or write).
func (c *otelMeterClient) instrumentCountLocked() int {
	return len(c.counters) + len(c.gauges) + len(c.histograms)
}

// rejectIfFullLocked returns errInstrumentLimit when creating a new
// instrument named name would exceed maxInstruments. Callers must hold
// c.mu for writing (K9).
func (c *otelMeterClient) rejectIfFullLocked(name string) error {
	if c.maxInstruments > 0 && c.instrumentCountLocked() >= c.maxInstruments {
		return fmt.Errorf(
			"otel-metrics: rejecting dynamic metric %q: %w (limit %d); use a bounded static name set",
			name, errInstrumentLimit, c.maxInstruments,
		)
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

	if err := c.rejectIfFullLocked(name); err != nil {
		return nil, err
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

	if err := c.rejectIfFullLocked(name); err != nil {
		return nil, err
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

	if err := c.rejectIfFullLocked(name); err != nil {
		return nil, err
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

// observedMetricExporter wraps a metric Exporter to surface export
// failures through an error callback that would otherwise be
// invisible (K3). All non-Export methods are inherited from the
// embedded Exporter interface.
type observedMetricExporter struct {
	sdkmetric.Exporter
	onError func(error)
}

func (e *observedMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil && e.onError != nil {
		e.onError(fmt.Errorf("otel-metrics: export: %w", err))
	}
	return err //nolint:wrapcheck // decorator pass-through: onError already wraps for observability; the sdkmetric.Exporter contract requires returning the SDK error verbatim.
}
