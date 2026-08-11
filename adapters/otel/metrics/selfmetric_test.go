package otelmetrics_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	otelmetrics "github.com/mariotoffia/gobridge/adapters/otel/metrics"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// rejected emissions must be visible through the exporter's own
// pipeline as the ExporterRejectedDatums self-metric, not only through
// the error handler.
func TestExporter_SelfMetric_ReportsRejectedDatums(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp,
		otelmetrics.WithMaxInstruments(1),
		otelmetrics.WithErrorHandler(nil), // opt out of warn logging in tests
	)

	e.Counter("test.fits", 1)    // fills the cache
	e.Counter("test.reject1", 1) // rejected
	e.Gauge("test.reject2", 1)   // rejected

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	data, ok := lookupMetric(rm, otelmetrics.MetricExporterRejectedDatums)
	require.True(t, ok, "self-metric must be published once rejections occur")
	sum, ok := data.(metricdata.Sum[int64])
	require.True(t, ok, "self-metric must be an int64 sum")
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, int64(2), sum.DataPoints[0].Value, "each rejected emission counts once")
}

// a healthy pipeline must not carry the self-loss series — nothing
// is observed while the rejected count is zero.
func TestExporter_SelfMetric_AbsentWithoutRejections(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp)
	e.Counter("test.ok", 1)

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	_, ok := lookupMetric(rm, otelmetrics.MetricExporterRejectedDatums)
	assert.False(t, ok, "self-metric must be absent while nothing was rejected")
}

// WithInstanceTag stamps every emitted metric with the
// instance_id attribute so fleet instances do not collide.
func TestExporter_InstanceTag_OnEveryMetric(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	e := otelmetrics.NewFromProvider(mp, otelmetrics.WithInstanceTag("bridge-7"))
	e.Counter("test.instance", 1, shared.Tag{Key: "route", Value: "orders"})

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	data, ok := lookupMetric(rm, "test.instance")
	require.True(t, ok)
	sum, ok := data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)

	found := false
	for _, attr := range sum.DataPoints[0].Attributes.ToSlice() {
		if string(attr.Key) == otelmetrics.TagKeyInstanceID && attr.Value.AsString() == "bridge-7" {
			found = true
		}
	}
	assert.True(t, found, "expected %s=bridge-7 attribute", otelmetrics.TagKeyInstanceID)
}

// an empty id derives a hostname-pid identity instead of silently
// dropping the tag.
func TestWithInstanceTag_DerivesWhenEmpty(t *testing.T) {
	t.Parallel()

	e := otelmetrics.NewForTest(otelmetrics.WithInstanceTag(""))
	cfg := e.ExportConfigForTest()
	require.NotEmpty(t, cfg.InstanceID, "empty id must derive hostname-pid")

	found := false
	for _, tag := range cfg.DefaultTags {
		if tag.Key == otelmetrics.TagKeyInstanceID && tag.Value == cfg.InstanceID {
			found = true
		}
	}
	assert.True(t, found, "derived instance id must join DefaultTags")
}

// lookupMetric returns the aggregation for name, reporting absence
// instead of failing so callers can assert either way.
func lookupMetric(rm *metricdata.ResourceMetrics, name string) (metricdata.Aggregation, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data, true
			}
		}
	}
	return nil, false
}
