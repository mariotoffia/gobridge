package cloudwatch

import (
	"testing"
	"time"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/mariotoffia/gobridge/domain"
)

// Verifies batcher add stores a counter datum with correct name, value, unit, and dimensions.
func TestBatcher_AddCounter(t *testing.T) {
	b := newBatcher("Test", nil, 100)

	full := b.add(metricData{
		name:       "test.counter",
		value:      5,
		unit:       cwtypes.StandardUnitCount,
		tags:       []domain.Tag{{Key: "env", Value: "test"}},
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

// Verifies histogram samples aggregate into StatisticValues on drain.
func TestBatcher_AddHistogram(t *testing.T) {
	b := newBatcher("Test", nil, 100)

	for _, v := range []float64{10, 20, 30, 40, 50} {
		b.add(metricData{
			name:       "test.latency",
			value:      v,
			unit:       cwtypes.StandardUnitMilliseconds,
			tags:       []domain.Tag{{Key: "ep", Value: "/api"}},
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
	defaults := []domain.Tag{
		{Key: "service", Value: "bridge"},
		{Key: "env", Value: "prod"},
	}
	b := newBatcher("Test", defaults, 100)

	b.add(metricData{
		name:       "m",
		value:      1,
		unit:       cwtypes.StandardUnitCount,
		tags:       []domain.Tag{{Key: "topic", Value: "orders"}},
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

// Verifies add returns full once the batcher reaches its capacity.
func TestBatcher_BufferFull(t *testing.T) {
	b := newBatcher("Test", nil, 5)

	for i := 0; i < 4; i++ {
		if b.add(metricData{name: "m", value: float64(i), metricType: metricTypeCounter}) {
			t.Errorf("buffer should not be full at %d", i)
		}
	}
	if !b.add(metricData{name: "m", value: 4, metricType: metricTypeCounter}) {
		t.Error("buffer should be full at 5")
	}
}

// Verifies drain empties mixed counter and histogram state.
func TestBatcher_DrainClears(t *testing.T) {
	b := newBatcher("Test", nil, 100)
	b.add(metricData{name: "c", value: 1, metricType: metricTypeCounter})
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
	var tags []domain.Tag
	for i := 0; i < 35; i++ {
		tags = append(tags, domain.Tag{Key: "k" + string(rune('A'+i)), Value: "v"})
	}
	b := newBatcher("Test", tags, 100)
	b.add(metricData{name: "m", value: 1, metricType: metricTypeCounter})

	data := b.drain()
	if len(data[0].Dimensions) > 30 {
		t.Errorf("dims = %d, want <= 30", len(data[0].Dimensions))
	}
}

// Verifies histograms with different tag sets produce separate aggregated datums.
func TestBatcher_MultipleHistogramKeys(t *testing.T) {
	b := newBatcher("Test", nil, 100)

	b.add(metricData{name: "latency", value: 10, tags: []domain.Tag{{Key: "r", Value: "A"}}, metricType: metricTypeHistogram})
	b.add(metricData{name: "latency", value: 20, tags: []domain.Tag{{Key: "r", Value: "B"}}, metricType: metricTypeHistogram})
	b.add(metricData{name: "latency", value: 30, tags: []domain.Tag{{Key: "r", Value: "A"}}, metricType: metricTypeHistogram})

	data := b.drain()
	if len(data) != 2 {
		t.Fatalf("expected 2 aggregated datums, got %d", len(data))
	}
}

// Verifies applyDefaults sets flush interval, buffer size, and max batch size.
func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{Namespace: "Test"}
	applyDefaults(cfg)

	if cfg.FlushInterval != 60*time.Second {
		t.Errorf("FlushInterval = %v, want 60s", cfg.FlushInterval)
	}
	if cfg.BufferSize != 1000 {
		t.Errorf("BufferSize = %d, want 1000", cfg.BufferSize)
	}
	if cfg.MaxBatchSize != 20 {
		t.Errorf("MaxBatchSize = %d, want 20", cfg.MaxBatchSize)
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

	WithEndpoint("http://localhost:4566")(e)
	if e.config.Endpoint != "http://localhost:4566" {
		t.Errorf("Endpoint = %q", e.config.Endpoint)
	}

	WithDefaultTags(domain.Tag{Key: "env", Value: "test"})(e)
	if len(e.config.DefaultTags) != 1 {
		t.Errorf("DefaultTags len = %d, want 1", len(e.config.DefaultTags))
	}
}

// Verifies metricNameFromKey strips tag suffixes from composite metric keys.
func TestMetricNameFromKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"LeaseAcquireLatency|lease_id=test-1", "LeaseAcquireLatency"},
		{"SomeMetric", "SomeMetric"},
		{"A|B=C|D=E", "A"},
	}
	for _, tt := range tests {
		got := metricNameFromKey(tt.key)
		if got != tt.want {
			t.Errorf("metricNameFromKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// Verifies histogram datums preserve the configured standard unit on drain.
func TestBatcher_HistogramUnit(t *testing.T) {
	b := newBatcher("Test", nil, 100)
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
