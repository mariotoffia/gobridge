package cloudwatch

import (
	"testing"
	"time"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// testBatcher builds a batcher with a deterministic fake clock and the
// given flush-trigger threshold. maxBuffered/maxRetry stay unbounded
// unless the test overrides cfg fields explicitly.
func testBatcher(bufferSize int) *batcher {
	return newBatcher(Config{
		BufferSize: bufferSize,
		Clock:      clocktest.NewAt(time.Unix(1700000000, 0)),
	})
}

// Verifies batcher add stores a counter datum with correct name, value, unit, and dimensions.
func TestBatcher_AddCounter(t *testing.T) {
	b := testBatcher(100)

	full := b.add(metricData{
		name:       "test.counter",
		value:      5,
		unit:       cwtypes.StandardUnitCount,
		tags:       []shared.Tag{{Key: "env", Value: "test"}},
		metricType: metricTypeCounter,
	})
	if full {
		t.Error("buffer should not be full")
	}
	if b.isEmpty() {
		t.Error("buffer should not be empty")
	}

	data := b.drain()
	if len(data) != 1 {
		t.Fatalf("expected 1 datum, got %d", len(data))
	}
	if *data[0].MetricName != "test.counter" {
		t.Errorf("name = %q, want test.counter", *data[0].MetricName)
	}
	if *data[0].Value != 5 {
		t.Errorf("value = %f, want 5", *data[0].Value)
	}
}

// regression: counter increments for the same (name, tags) aggregate
// into ONE summed datum per flush window instead of one datum per increment.
func TestBatcher_CounterAggregatesPerNameAndTags(t *testing.T) {
	b := testBatcher(100)

	for i := 0; i < 500; i++ {
		b.addCounter("requests", 1, []shared.Tag{{Key: "route_id", Value: "orders"}})
	}
	b.addCounter("requests", 2, []shared.Tag{{Key: "route_id", Value: "invoices"}})

	data := b.drain()
	if len(data) != 2 {
		t.Fatalf("expected 2 aggregated counter datums, got %d", len(data))
	}
	byDim := map[string]float64{}
	for _, d := range data {
		if len(d.Dimensions) != 1 {
			t.Fatalf("expected 1 dimension, got %d", len(d.Dimensions))
		}
		byDim[*d.Dimensions[0].Value] = *d.Value
	}
	if byDim["orders"] != 500 {
		t.Errorf("orders sum = %f, want 500", byDim["orders"])
	}
	if byDim["invoices"] != 2 {
		t.Errorf("invoices sum = %f, want 2", byDim["invoices"])
	}
}

// Verifies histogram samples aggregate into StatisticValues on drain.
func TestBatcher_AddHistogram(t *testing.T) {
	b := testBatcher(100)

	for _, v := range []float64{10, 20, 30, 40, 50} {
		b.add(metricData{
			name:       "test.latency",
			value:      v,
			unit:       cwtypes.StandardUnitMilliseconds,
			tags:       []shared.Tag{{Key: "ep", Value: "/api"}},
			metricType: metricTypeHistogram,
		})
	}

	data := b.drain()
	if len(data) != 1 {
		t.Fatalf("expected 1 aggregated datum, got %d", len(data))
	}
	s := data[0].StatisticValues
	if s == nil {
		t.Fatal("expected StatisticValues")
	}
	if *s.SampleCount != 5 {
		t.Errorf("count = %f, want 5", *s.SampleCount)
	}
	if *s.Sum != 150 {
		t.Errorf("sum = %f, want 150", *s.Sum)
	}
	if *s.Minimum != 10 {
		t.Errorf("min = %f, want 10", *s.Minimum)
	}
	if *s.Maximum != 50 {
		t.Errorf("max = %f, want 50", *s.Maximum)
	}
}

// Verifies default tags merge with per-metric tags in CloudWatch dimensions.
func TestBatcher_DefaultTags(t *testing.T) {
	b := newBatcher(Config{
		BufferSize: 100,
		Clock:      clocktest.NewAt(time.Unix(1700000000, 0)),
		DefaultTags: []shared.Tag{
			{Key: "service", Value: "bridge"},
			{Key: "env", Value: "prod"},
		},
	})

	b.add(metricData{
		name:       "m",
		value:      1,
		unit:       cwtypes.StandardUnitCount,
		tags:       []shared.Tag{{Key: "topic", Value: "orders"}},
		metricType: metricTypeCounter,
	})

	data := b.drain()
	if len(data[0].Dimensions) != 3 {
		t.Errorf("dims = %d, want 3", len(data[0].Dimensions))
	}

	found := map[string]string{}
	for _, d := range data[0].Dimensions {
		found[*d.Name] = *d.Value
	}
	for _, want := range []struct{ k, v string }{
		{"service", "bridge"}, {"env", "prod"}, {"topic", "orders"},
	} {
		if found[want.k] != want.v {
			t.Errorf("dim %s = %q, want %q", want.k, found[want.k], want.v)
		}
	}
}

// Verifies add returns full once the pending state reaches the trigger
// threshold. Counters aggregate per (name, tags), so distinct names are
// used to grow the pending series count.
func TestBatcher_BufferFull(t *testing.T) {
	b := testBatcher(5)

	names := []string{"m1", "m2", "m3", "m4"}
	for i, n := range names {
		if b.add(metricData{name: n, value: float64(i), metricType: metricTypeCounter}) {
			t.Errorf("buffer should not be full at %d", i)
		}
	}
	if !b.add(metricData{name: "m5", value: 4, metricType: metricTypeCounter}) {
		t.Error("buffer should be full at 5")
	}
}

// Verifies drain empties mixed counter, gauge, and histogram state.
func TestBatcher_DrainClears(t *testing.T) {
	b := testBatcher(100)
	b.add(metricData{name: "c", value: 1, metricType: metricTypeCounter})
	b.add(metricData{name: "g", value: 2, metricType: metricTypeGauge})
	b.add(metricData{name: "h", value: 10, metricType: metricTypeHistogram})

	if b.isEmpty() {
		t.Error("should not be empty before drain")
	}
	b.drain()
	if !b.isEmpty() {
		t.Error("should be empty after drain")
	}
}

// Verifies dimensions are capped at CloudWatch's maximum count.
func TestBatcher_DimensionLimit(t *testing.T) {
	var tags []shared.Tag
	for i := 0; i < 35; i++ {
		tags = append(tags, shared.Tag{Key: "k" + string(rune('A'+i)), Value: "v"})
	}
	b := newBatcher(Config{
		BufferSize:  100,
		Clock:       clocktest.NewAt(time.Unix(1700000000, 0)),
		DefaultTags: tags,
	})
	b.add(metricData{name: "m", value: 1, metricType: metricTypeCounter})

	data := b.drain()
	if len(data[0].Dimensions) > 30 {
		t.Errorf("dims = %d, want <= 30", len(data[0].Dimensions))
	}
}

// Verifies histograms with different tag sets produce separate aggregated datums.
func TestBatcher_MultipleHistogramKeys(t *testing.T) {
	b := testBatcher(100)

	b.add(metricData{name: "latency", value: 10, tags: []shared.Tag{{Key: "r", Value: "A"}}, metricType: metricTypeHistogram})
	b.add(metricData{name: "latency", value: 20, tags: []shared.Tag{{Key: "r", Value: "B"}}, metricType: metricTypeHistogram})
	b.add(metricData{name: "latency", value: 30, tags: []shared.Tag{{Key: "r", Value: "A"}}, metricType: metricTypeHistogram})

	data := b.drain()
	if len(data) != 2 {
		t.Fatalf("expected 2 aggregated datums, got %d", len(data))
	}
}

// regression: the aggregation key must not fold distinct series whose
// name/tag concatenations collide under a naive separator-joined encoding.
func TestSeriesKey_NoAmbiguity(t *testing.T) {
	a := seriesKey("m", []shared.Tag{{Key: "a", Value: "1|b=2"}}, false)
	b := seriesKey("m", []shared.Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}, false)
	if a == b {
		t.Errorf("distinct tag sets folded to the same key %q", a)
	}

	c := seriesKey("m|x=y", nil, false)
	d := seriesKey("m", []shared.Tag{{Key: "x", Value: "y"}}, false)
	if c == d {
		t.Errorf("name containing separator folded with tagged series: %q", c)
	}

	// The rollup copy of a series must never collide with the primary.
	e := seriesKey("m", nil, true)
	f := seriesKey("m", nil, false)
	if e == f {
		t.Errorf("rollup key must differ from primary key: %q", e)
	}
}

// names and tags are preserved verbatim on the aggregate structs —
// a metric name containing the old "|" separator survives drain intact.
func TestBatcher_SeparatorInNameSurvivesDrain(t *testing.T) {
	b := testBatcher(100)
	b.add(metricData{name: "weird|name", value: 1, metricType: metricTypeHistogram})

	data := b.drain()
	if len(data) != 1 {
		t.Fatalf("expected 1 datum, got %d", len(data))
	}
	if *data[0].MetricName != "weird|name" {
		t.Errorf("name = %q, want weird|name", *data[0].MetricName)
	}
}

// Verifies applyDefaults sets flush interval, buffer size, and the
// real CloudWatch API batch limits.
func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{Namespace: "Test"}
	applyDefaults(cfg)

	if cfg.FlushInterval != 60*time.Second {
		t.Errorf("FlushInterval = %v, want 60s", cfg.FlushInterval)
	}
	if cfg.BufferSize != 1000 {
		t.Errorf("BufferSize = %d, want 1000", cfg.BufferSize)
	}
	if cfg.MaxBatchSize != apiMaxBatchDatums {
		t.Errorf("MaxBatchSize = %d, want %d (CloudWatch API limit)", cfg.MaxBatchSize, apiMaxBatchDatums)
	}
	if cfg.MaxBatchBytes != 900_000 {
		t.Errorf("MaxBatchBytes = %d, want 900000", cfg.MaxBatchBytes)
	}
	if cfg.MaxBufferedDatums != 10000 {
		t.Errorf("MaxBufferedDatums = %d, want 10000", cfg.MaxBufferedDatums)
	}
}

// a configured MaxBatchSize above the API hard limit is clamped.
func TestConfig_ClampsMaxBatchSizeToAPILimit(t *testing.T) {
	cfg := &Config{MaxBatchSize: 5000, MaxBatchBytes: 5_000_000}
	applyDefaults(cfg)
	if cfg.MaxBatchSize != apiMaxBatchDatums {
		t.Errorf("MaxBatchSize = %d, want clamped to %d", cfg.MaxBatchSize, apiMaxBatchDatums)
	}
	if cfg.MaxBatchBytes > apiMaxBatchBytes {
		t.Errorf("MaxBatchBytes = %d, want clamped to <= %d", cfg.MaxBatchBytes, apiMaxBatchBytes)
	}
}

// Verifies functional options mutate Exporter configuration fields as expected.
func TestOptions(t *testing.T) {
	e := &Exporter{config: Config{}}

	WithRegion("eu-west-1")(e)
	if e.config.Region != "eu-west-1" {
		t.Errorf("Region = %q, want eu-west-1", e.config.Region)
	}

	WithNamespace("Custom/NS")(e)
	if e.config.Namespace != "Custom/NS" {
		t.Errorf("Namespace = %q, want Custom/NS", e.config.Namespace)
	}

	WithFlushInterval(30 * time.Second)(e)
	if e.config.FlushInterval != 30*time.Second {
		t.Errorf("FlushInterval = %v, want 30s", e.config.FlushInterval)
	}

	WithBufferSize(500)(e)
	if e.config.BufferSize != 500 {
		t.Errorf("BufferSize = %d, want 500", e.config.BufferSize)
	}

	WithMaxBatchSize(100)(e)
	if e.config.MaxBatchSize != 100 {
		t.Errorf("MaxBatchSize = %d, want 100", e.config.MaxBatchSize)
	}

	WithMaxBufferedDatums(2000)(e)
	if e.config.MaxBufferedDatums != 2000 {
		t.Errorf("MaxBufferedDatums = %d, want 2000", e.config.MaxBufferedDatums)
	}

	WithEndpoint("http://localhost:4566")(e)
	if e.config.Endpoint != "http://localhost:4566" {
		t.Errorf("Endpoint = %q", e.config.Endpoint)
	}

	WithDefaultTags(shared.Tag{Key: "env", Value: "test"})(e)
	if len(e.config.DefaultTags) != 1 {
		t.Errorf("DefaultTags len = %d, want 1", len(e.config.DefaultTags))
	}

	WithRollupMetrics("OutboxDepth", "DLQEntries")(e)
	if len(e.config.RollupMetrics) != 2 {
		t.Errorf("RollupMetrics len = %d, want 2", len(e.config.RollupMetrics))
	}

	WithInstanceTag("bridge-1")(e)
	if e.config.InstanceID != "bridge-1" {
		t.Errorf("InstanceID = %q, want bridge-1", e.config.InstanceID)
	}
}

// WithInstanceTag("") auto-derives a hostname-pid identity.
func TestWithInstanceTag_DerivesWhenEmpty(t *testing.T) {
	e := &Exporter{config: Config{}}
	WithInstanceTag("")(e)
	if e.config.InstanceID == "" {
		t.Fatal("expected derived non-empty InstanceID")
	}
}

// an InstanceID becomes an instance_id dimension on every
// dimensioned datum via DefaultTags.
func TestApplyDefaults_InstanceTagJoinsDefaultTags(t *testing.T) {
	cfg := &Config{InstanceID: "bridge-7"}
	applyDefaults(cfg)
	if !hasTagKey(cfg.DefaultTags, TagKeyInstanceID) {
		t.Fatalf("expected %s in DefaultTags, got %v", TagKeyInstanceID, cfg.DefaultTags)
	}
	// Idempotent: applying twice must not duplicate the tag.
	applyDefaults(cfg)
	count := 0
	for _, tag := range cfg.DefaultTags {
		if tag.Key == TagKeyInstanceID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("instance_id tag appears %d times, want 1", count)
	}
}

// Verifies histogram datums preserve the configured standard unit on drain.
func TestBatcher_HistogramUnit(t *testing.T) {
	b := testBatcher(100)
	b.add(metricData{
		name:       "timer",
		value:      42,
		unit:       cwtypes.StandardUnitMilliseconds,
		metricType: metricTypeHistogram,
	})

	data := b.drain()
	if data[0].Unit != cwtypes.StandardUnitMilliseconds {
		t.Errorf("unit = %v, want Milliseconds", data[0].Unit)
	}
}
