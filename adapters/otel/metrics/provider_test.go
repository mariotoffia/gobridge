package otelmetrics_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	otelmetrics "github.com/mariotoffia/gobridge/adapters/otel/metrics"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var _ ports.MetricsExporter = (*otelmetrics.Exporter)(nil)

// Verifies NewForTest creates an exporter with defaults applied without network.
func TestNewForTest_Defaults(t *testing.T) {
	t.Parallel()

	e := otelmetrics.NewForTest()
	cfg := e.ExportConfigForTest()

	assert.Equal(t, "http://localhost:4318", cfg.Endpoint)
	assert.Equal(t, "gobridge", cfg.ServiceName)
	assert.Equal(t, 60*time.Second, cfg.FlushInterval)
}

// Verifies NewForTest preserves explicitly set options.
func TestNewForTest_Options(t *testing.T) {
	t.Parallel()

	e := otelmetrics.NewForTest(
		otelmetrics.WithEndpoint("http://collector:4318"),
		otelmetrics.WithServiceName("my-svc"),
		otelmetrics.WithFlushInterval(10*time.Second),
		otelmetrics.WithInsecure(),
		otelmetrics.WithHeaders(map[string]string{"X-Key": "val"}),
	)
	cfg := e.ExportConfigForTest()

	assert.Equal(t, "http://collector:4318", cfg.Endpoint)
	assert.Equal(t, "my-svc", cfg.ServiceName)
	assert.Equal(t, 10*time.Second, cfg.FlushInterval)
	assert.True(t, cfg.Insecure)
	assert.Equal(t, map[string]string{"X-Key": "val"}, cfg.Headers)
}

// Verifies Counter records an additive measurement via the SDK manual reader.
func TestExporter_Counter(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Counter("test.requests", 5)
	e.Counter("test.requests", 3)

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.requests")
	require.IsType(t, metricdata.Sum[int64]{}, dp)

	sum := dp.(metricdata.Sum[int64])
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, int64(8), sum.DataPoints[0].Value)
}

// Verifies Gauge records the latest value via the SDK manual reader.
func TestExporter_Gauge(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Gauge("test.cpu", 42.5)
	e.Gauge("test.cpu", 87.3)

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.cpu")
	require.IsType(t, metricdata.Gauge[float64]{}, dp)

	gauge := dp.(metricdata.Gauge[float64])
	require.Len(t, gauge.DataPoints, 1)
	assert.Equal(t, 87.3, gauge.DataPoints[0].Value)
}

// Verifies Histogram records values into a distribution.
func TestExporter_Histogram(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Histogram("test.latency", 10)
	e.Histogram("test.latency", 20)
	e.Histogram("test.latency", 30)

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.latency")
	require.IsType(t, metricdata.Histogram[float64]{}, dp)

	hist := dp.(metricdata.Histogram[float64])
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, uint64(3), hist.DataPoints[0].Count)
	assert.Equal(t, 60.0, hist.DataPoints[0].Sum)

	minVal, minDefined := hist.DataPoints[0].Min.Value()
	require.True(t, minDefined, "Min should be defined")
	assert.Equal(t, 10.0, minVal)

	maxVal, maxDefined := hist.DataPoints[0].Max.Value()
	require.True(t, maxDefined, "Max should be defined")
	assert.Equal(t, 30.0, maxVal)
}

// Verifies Timer records duration as milliseconds into a histogram.
func TestExporter_Timer(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Timer("test.duration", 250*time.Millisecond)

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.duration")
	require.IsType(t, metricdata.Histogram[float64]{}, dp)

	hist := dp.(metricdata.Histogram[float64])
	require.Len(t, hist.DataPoints, 1)
	assert.InDelta(t, 250.0, hist.DataPoints[0].Sum, 1.0)
}

// Verifies domain tags are forwarded as OTel attributes.
func TestExporter_Counter_WithTags(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Counter("test.tagged", 1, shared.Tag{Key: "route", Value: "orders"})
	e.Counter("test.tagged", 1, shared.Tag{Key: "route", Value: "payments"})

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.tagged")
	require.IsType(t, metricdata.Sum[int64]{}, dp)

	sum := dp.(metricdata.Sum[int64])
	assert.Len(t, sum.DataPoints, 2, "different tag sets produce different data points")
}

// Verifies default tags are attached to every metric.
func TestExporter_DefaultTags(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp,
		otelmetrics.WithDefaultTags(shared.Tag{Key: "env", Value: "test"}),
	)
	e.Counter("test.m", 1)

	rm := collectMetrics(t, reader)
	dp := findMetric(t, rm, "test.m")
	require.IsType(t, metricdata.Sum[int64]{}, dp)

	sum := dp.(metricdata.Sum[int64])
	require.Len(t, sum.DataPoints, 1)

	found := false
	for _, attr := range sum.DataPoints[0].Attributes.ToSlice() {
		if string(attr.Key) == "env" && attr.Value.AsString() == "test" {
			found = true
		}
	}
	assert.True(t, found, "expected default tag env=test on metric")
}

// Verifies Flush forces metric collection via the provider.
func TestExporter_Flush(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Counter("test.flush", 1)

	err := e.Flush(context.Background())
	require.NoError(t, err)
}

// Verifies Close shuts down the provider without error.
func TestExporter_Close(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	e := otelmetrics.NewFromProvider(mp)
	e.Counter("test.close", 1)

	err := e.Close(context.Background())
	require.NoError(t, err)
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) *metricdata.ResourceMetrics {
	t.Helper()
	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))
	return rm
}

func findMetric(t *testing.T, rm *metricdata.ResourceMetrics, name string) metricdata.Aggregation {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("metric %q not found in collected data", name)
	return nil
}
