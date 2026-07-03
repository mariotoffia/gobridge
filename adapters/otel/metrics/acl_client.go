package otelmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// errInstrumentLimit is returned when the instrument cache is full and
// a new (likely dynamic) metric name is rejected (K9).
var errInstrumentLimit = errors.New("instrument cache limit reached")

// rejectReportInterval bounds how often a full-cache rejection is
// surfaced through the error handler (N4). Once the cache is full every
// new dynamic name is rejected on every emit; reporting each one would
// flood the handler, so reports are throttled to one per interval.
const rejectReportInterval = time.Minute

// MetricExporterRejectedDatums is the self-metric (MF-5) reporting the
// cumulative number of metric emissions this exporter rejected (full
// instrument cache, K9). Published through the exporter's own pipeline
// as an observable counter so silent self-loss is visible on the same
// backend as the metrics themselves. Name kept in sync with the
// CloudWatch adapter's equivalent self-metric.
const MetricExporterRejectedDatums = "ExporterRejectedDatums"

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

	// clk sources the timestamp for reject-report throttling. Defaults to
	// clock.System; the codebase forbids direct time.Now (forbidigo).
	clk clock.Clock

	// lastRejectReport holds the UnixNano of the last full-cache
	// rejection surfaced through onError. It rate-limits rejection
	// reporting (N4) to one report per rejectReportInterval via a
	// lock-free CAS, so a dynamic-name flood neither floods the handler
	// nor contends on mu. Bounded memory: one timestamp, no per-name state.
	lastRejectReport atomic.Int64

	// rejectedTotal counts every rejected emission. It backs the
	// MetricExporterRejectedDatums observable self-metric (MF-5) so
	// self-loss is visible through the exporter's own pipeline, not
	// only through the (possibly suppressed) error handler.
	rejectedTotal atomic.Int64

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
	c := &otelMeterClient{
		provider:       mp,
		meter:          mp.Meter("github.com/mariotoffia/gobridge"),
		defaultAttrs:   buildDefaultAttrs(cfg.DefaultTags),
		maxInstruments: maxInstruments,
		onError:        cfg.errorHandler,
		clk:            clock.System,
		counters:       make(map[string]metric.Int64Counter),
		gauges:         make(map[string]metric.Float64Gauge),
		histograms:     make(map[string]metric.Float64Histogram),
	}
	c.registerSelfMetrics()
	return c
}

// registerSelfMetrics installs the MetricExporterRejectedDatums
// observable counter (MF-5). The callback observes the cumulative
// reject count on every collection; nothing is observed while the
// count is zero so healthy pipelines carry no extra series. Failure to
// create the instrument is surfaced through the error handler — the
// exporter itself stays functional.
func (c *otelMeterClient) registerSelfMetrics() {
	_, err := c.meter.Int64ObservableCounter(MetricExporterRejectedDatums,
		metric.WithDescription("metric emissions rejected by the exporter (instrument cache full)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if v := c.rejectedTotal.Load(); v > 0 {
				o.Observe(v)
			}
			return nil
		}),
	)
	if err != nil && c.onError != nil {
		c.onError(fmt.Errorf("otel-metrics: create self-metric %s: %w", MetricExporterRejectedDatums, err))
	}
}

// reportInstrumentError surfaces an instrument-acquisition failure through
// the configured error handler. The port methods have no error return, so
// without a handler these failures are lost; the handler is the classified
// visibility path (K3).
//
// A full-cache rejection (errInstrumentLimit) is reported at most once per
// rejectReportInterval (N4): under dynamic-name misuse every emit is
// rejected, and reporting each one would flood the handler. The formatted
// error is built only when a report is actually emitted, so the rejected
// hot path stays allocation-free. Bounded memory: a single timestamp, no
// per-name state. Other (rare) acquisition errors are always reported.
func (c *otelMeterClient) reportInstrumentError(name string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errInstrumentLimit) {
		c.rejectedTotal.Add(1)
	}
	if c.onError == nil {
		return
	}
	if errors.Is(err, errInstrumentLimit) {
		if !c.allowRejectReport() {
			return
		}
		err = fmt.Errorf(
			"otel-metrics: rejecting dynamic metric %q: %w (limit %d); use a bounded static name set",
			name, errInstrumentLimit, c.maxInstruments,
		)
	}
	c.onError(err)
}

// allowRejectReport reports whether a full-cache rejection may be surfaced
// now, throttling to one report per rejectReportInterval. It is lock-free
// (a single load plus CAS) so the rejected hot path never contends on
// c.mu, and concurrent floods yield exactly one report per window (N4).
func (c *otelMeterClient) allowRejectReport() bool {
	now := c.clk.Now().UnixNano()
	last := c.lastRejectReport.Load()
	if now-last < int64(rejectReportInterval) {
		return false
	}
	return c.lastRejectReport.CompareAndSwap(last, now)
}

func (c *otelMeterClient) Counter(name string, value int64, tags []shared.Tag) {
	counter, err := c.getOrCreateCounter(name)
	if err != nil {
		c.reportInstrumentError(name, err)
		return
	}
	counter.Add(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Gauge(name string, value float64, tags []shared.Tag) {
	gauge, err := c.getOrCreateGauge(name)
	if err != nil {
		c.reportInstrumentError(name, err)
		return
	}
	gauge.Record(context.Background(), value, metric.WithAttributes(c.buildAttributes(tags)...))
}

func (c *otelMeterClient) Histogram(name string, value float64, tags []shared.Tag) {
	histogram, err := c.getOrCreateHistogram(name)
	if err != nil {
		c.reportInstrumentError(name, err)
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

// isFullLocked reports whether the instrument cache has reached
// maxInstruments. Callers must hold c.mu (read or write). The caches only
// grow (there is no eviction), so once this is true it stays true — the
// RLock fast path (N4) relies on that to reject a new name without taking
// the write Lock. maxInstruments <= 0 disables the bound.
func (c *otelMeterClient) isFullLocked() bool {
	return c.maxInstruments > 0 && c.instrumentCountLocked() >= c.maxInstruments
}

func (c *otelMeterClient) getOrCreateCounter(name string) (metric.Int64Counter, error) {
	c.mu.RLock()
	counter, ok := c.counters[name]
	full := !ok && c.isFullLocked()
	c.mu.RUnlock()
	if ok {
		return counter, nil
	}
	if full {
		// N4 fast path: the cache is full and this name is not cached.
		// Because the caches only grow, the name will always be rejected;
		// return the sentinel without taking the write Lock so a
		// dynamic-name flood cannot serialize the hot path. The formatted
		// error is built later, only if a report is actually emitted.
		return nil, errInstrumentLimit
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if counter, ok := c.counters[name]; ok {
		return counter, nil
	}

	if c.isFullLocked() {
		return nil, errInstrumentLimit
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
	gauge, ok := c.gauges[name]
	full := !ok && c.isFullLocked()
	c.mu.RUnlock()
	if ok {
		return gauge, nil
	}
	if full {
		// N4 fast path: see getOrCreateCounter.
		return nil, errInstrumentLimit
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if gauge, ok := c.gauges[name]; ok {
		return gauge, nil
	}

	if c.isFullLocked() {
		return nil, errInstrumentLimit
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
	histogram, ok := c.histograms[name]
	full := !ok && c.isFullLocked()
	c.mu.RUnlock()
	if ok {
		return histogram, nil
	}
	if full {
		// N4 fast path: see getOrCreateCounter.
		return nil, errInstrumentLimit
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if histogram, ok := c.histograms[name]; ok {
		return histogram, nil
	}

	if c.isFullLocked() {
		return nil, errInstrumentLimit
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
