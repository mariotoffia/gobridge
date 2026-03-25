package otelmetrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

var _ ports.MetricsExporter = (*Exporter)(nil)

// Exporter implements ports.MetricsExporter for OpenTelemetry.
type Exporter struct {
	config       Config
	provider     *sdkmetric.MeterProvider
	meter        metric.Meter
	counters     map[string]metric.Int64Counter
	gauges       map[string]metric.Float64Gauge
	histograms   map[string]metric.Float64Histogram
	defaultAttrs []attribute.KeyValue
	mu           sync.RWMutex
}

// New creates a new OTEL metrics exporter backed by an OTLP HTTP
// exporter. The returned Exporter is safe for concurrent use.
func New(ctx context.Context, opts ...Option) (*Exporter, error) {
	e := &Exporter{
		counters:   make(map[string]metric.Int64Counter),
		gauges:     make(map[string]metric.Float64Gauge),
		histograms: make(map[string]metric.Float64Histogram),
	}

	for _, opt := range opts {
		opt(e)
	}

	applyDefaults(&e.config)

	e.defaultAttrs = make([]attribute.KeyValue, len(e.config.DefaultTags))
	for i, tag := range e.config.DefaultTags {
		e.defaultAttrs[i] = attribute.String(tag.Key, tag.Value)
	}

	exporterOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(e.config.Endpoint),
	}

	if e.config.Insecure {
		exporterOpts = append(exporterOpts, otlpmetrichttp.WithInsecure())
	}

	if len(e.config.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlpmetrichttp.WithHeaders(e.config.Headers))
	}

	exporter, err := otlpmetrichttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(e.config.ServiceName),
			semconv.ServiceVersion(e.config.ServiceVersion),
			semconv.DeploymentEnvironment(e.config.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	e.provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(
				exporter,
				sdkmetric.WithInterval(e.config.FlushInterval),
			),
		),
		sdkmetric.WithResource(res),
	)

	e.meter = e.provider.Meter("github.com/mariotoffia/gobridge")

	return e, nil
}

// Counter increments a counter metric.
func (e *Exporter) Counter(name string, value int64, tags ...domain.Tag) {
	counter, err := e.getOrCreateCounter(name)
	if err != nil {
		return
	}

	attrs := e.buildAttributes(tags)
	counter.Add(context.Background(), value, metric.WithAttributes(attrs...))
}

// Gauge sets a gauge metric value.
func (e *Exporter) Gauge(name string, value float64, tags ...domain.Tag) {
	gauge, err := e.getOrCreateGauge(name)
	if err != nil {
		return
	}

	attrs := e.buildAttributes(tags)
	gauge.Record(context.Background(), value, metric.WithAttributes(attrs...))
}

// Histogram records a histogram value.
func (e *Exporter) Histogram(name string, value float64, tags ...domain.Tag) {
	histogram, err := e.getOrCreateHistogram(name)
	if err != nil {
		return
	}

	attrs := e.buildAttributes(tags)
	histogram.Record(context.Background(), value, metric.WithAttributes(attrs...))
}

// Timer records a duration as milliseconds into a histogram.
func (e *Exporter) Timer(name string, duration time.Duration, tags ...domain.Tag) {
	ms := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	e.Histogram(name, ms, tags...)
}

// Flush forces a metric export.
func (e *Exporter) Flush(ctx context.Context) error {
	return e.provider.ForceFlush(ctx)
}

// Close shuts down the exporter.
func (e *Exporter) Close(ctx context.Context) error {
	return e.provider.Shutdown(ctx)
}

func (e *Exporter) getOrCreateCounter(name string) (metric.Int64Counter, error) {
	e.mu.RLock()
	if counter, ok := e.counters[name]; ok {
		e.mu.RUnlock()
		return counter, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if counter, ok := e.counters[name]; ok {
		return counter, nil
	}

	counter, err := e.meter.Int64Counter(name)
	if err != nil {
		return nil, err
	}

	e.counters[name] = counter
	return counter, nil
}

func (e *Exporter) getOrCreateGauge(name string) (metric.Float64Gauge, error) {
	e.mu.RLock()
	if gauge, ok := e.gauges[name]; ok {
		e.mu.RUnlock()
		return gauge, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if gauge, ok := e.gauges[name]; ok {
		return gauge, nil
	}

	gauge, err := e.meter.Float64Gauge(name)
	if err != nil {
		return nil, err
	}

	e.gauges[name] = gauge
	return gauge, nil
}

func (e *Exporter) getOrCreateHistogram(name string) (metric.Float64Histogram, error) {
	e.mu.RLock()
	if histogram, ok := e.histograms[name]; ok {
		e.mu.RUnlock()
		return histogram, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if histogram, ok := e.histograms[name]; ok {
		return histogram, nil
	}

	histogram, err := e.meter.Float64Histogram(name)
	if err != nil {
		return nil, err
	}

	e.histograms[name] = histogram
	return histogram, nil
}

func (e *Exporter) buildAttributes(tags []domain.Tag) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(e.defaultAttrs)+len(tags))
	attrs = append(attrs, e.defaultAttrs...)

	for _, tag := range tags {
		attrs = append(attrs, attribute.String(tag.Key, tag.Value))
	}

	return attrs
}
