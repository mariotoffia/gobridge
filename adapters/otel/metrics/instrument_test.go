package otelmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// K3 true-regression: metric export failures are surfaced via the error
// handler instead of being silently swallowed.
func TestObservedMetricExporter_ReportsExportError(t *testing.T) {
	t.Parallel()

	var got error
	obs := &observedMetricExporter{
		Exporter: failingMetricExporter{},
		onError:  func(err error) { got = err },
	}

	err := obs.Export(context.Background(), &metricdata.ResourceMetrics{})
	require.Error(t, err)
	require.Error(t, got, "error handler must observe the export failure")
	assert.Contains(t, got.Error(), "otel-metrics: export")
}

// A nil error handler must not panic.
func TestObservedMetricExporter_NilHandlerNoPanic(t *testing.T) {
	t.Parallel()

	obs := &observedMetricExporter{Exporter: failingMetricExporter{}}
	assert.Error(t, obs.Export(context.Background(), &metricdata.ResourceMetrics{}))
}

// K9 true-regression: once the instrument cache is full, further dynamic
// metric names are rejected and surfaced via the error handler rather
// than growing the cache without bound.
func TestInstrumentLimit_RejectsDynamicNames(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	var errsSeen int
	var lastErr error
	client := newMeterClientFromProvider(mp, Config{
		MaxInstruments: 2,
		errorHandler: func(err error) {
			errsSeen++
			lastErr = err
		},
	})

	// Two distinct instruments fit within the bound.
	client.Counter("m1", 1, nil)
	client.Gauge("m2", 1, nil)
	require.Equal(t, 0, errsSeen, "instruments within the bound must not error")

	// A third distinct name is rejected.
	client.Histogram("m3", 1, nil)
	require.Equal(t, 1, errsSeen, "over-limit instrument must be reported once")
	require.ErrorIs(t, lastErr, errInstrumentLimit)
	assert.Contains(t, lastErr.Error(), "m3")

	// Re-emitting an already-cached instrument is unaffected.
	client.Counter("m1", 5, nil)
	assert.Equal(t, 1, errsSeen, "cached instrument re-emit must not error")
}

// N4 regression: once the instrument cache is full, a concurrent flood of
// NEW (rejected) dynamic names must not (a) flood the error handler nor
// (b) serialize the hot path on the write Lock. Rejection reporting is
// throttled to one report per rejectReportInterval, and the rejected fast
// path bypasses the write Lock entirely, so the emitters must complete
// without deadlocking. Run with -race to catch a bad fast-path change.
func TestInstrumentLimit_RejectFloodRateLimited(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	var reports atomic.Int64
	client := newMeterClientFromProvider(mp, Config{
		MaxInstruments: 2,
		errorHandler:   func(error) { reports.Add(1) },
	})

	// Fill the cache to its bound so every further distinct name is rejected.
	client.Counter("static.a", 1, nil)
	client.Gauge("static.b", 1, nil)
	require.Zero(t, reports.Load(), "instruments within the bound must not report")

	const (
		workers   = 8
		perWorker = 2000
	)
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					// Every name is new => rejected against the full cache.
					client.Counter(fmt.Sprintf("dyn.%d.%d", w, i), 1, nil)
				}
			}(w)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("rejected-emit flood deadlocked: reject fast path must not take the write Lock")
	}

	// The whole flood falls within one rejectReportInterval, so exactly one
	// rejection is surfaced despite workers*perWorker rejected emits.
	require.Equal(t, int64(1), reports.Load(),
		"rejection reporting must be rate-limited to one per window, not flooded per emit")

	// A cached name still records without error after the flood.
	client.Counter("static.a", 5, nil)
	require.Equal(t, int64(1), reports.Load(), "cached re-emit must not report")
}

type failingMetricExporter struct {
	sdkmetric.Exporter
}

func (failingMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return errors.New("boom")
}

// K7: env endpoint / service name suppress the hardcoded defaults.
func TestApplyDefaults_HonorsEnvEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.internal:4318")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := Config{}
	applyDefaults(&cfg)

	assert.Empty(t, cfg.Endpoint, "env endpoint must not be overridden by a hardcoded default")
}

func TestApplyDefaults_HonorsEnvServiceName(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "svc-from-env")

	cfg := Config{}
	applyDefaults(&cfg)

	assert.Empty(t, cfg.ServiceName, "env service name must not be overridden by a hardcoded default")
}
