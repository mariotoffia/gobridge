package cloudwatch

import (
	"testing"
	"time"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// CloudWatch Metrics Exporter Unit Tests
//
// Tests for batching, aggregation, and configuration.
// Integration tests with LocalStack are in integration_cloudwatch_test.go
// ═══════════════════════════════════════════════════════════════════════════

// TestBatcher_AddCounter validates counter metric buffering.
func TestBatcher_AddCounter(t *testing.T) {
	b := newBatcher("TestNamespace", nil, 100)

	md := metricData{
		name:       "test.counter",
		value:      5,
		unit:       cwtypes.StandardUnitCount,
		tags:       []types.Tag{{Key: "env", Value: "test"}},
		metricType: metricTypeCounter,
	}

	full := b.add(md)
	if full {
		t.Error("expected buffer not full")
	}

	if b.isEmpty() {
		t.Error("expected buffer not empty")
	}

	data := b.drain()
	if len(data) != 1 {
		t.Errorf("expected 1 metric, got %d", len(data))
	}

	if *data[0].MetricName != "test.counter" {
		t.Errorf("expected metric name test.counter, got %s", *data[0].MetricName)
	}
	if *data[0].Value != 5 {
		t.Errorf("expected value 5, got %f", *data[0].Value)
	}
}

// TestBatcher_AddHistogram validates histogram aggregation.
func TestBatcher_AddHistogram(t *testing.T) {
	b := newBatcher("TestNamespace", nil, 100)

	// Add multiple histogram values for same metric
	for _, v := range []float64{10, 20, 30, 40, 50} {
		b.add(metricData{
			name:       "test.latency",
			value:      v,
			unit:       cwtypes.StandardUnitMilliseconds,
			tags:       []types.Tag{{Key: "endpoint", Value: "/api"}},
			metricType: metricTypeHistogram,
		})
	}

	data := b.drain()
	if len(data) != 1 {
		t.Errorf("expected 1 aggregated metric, got %d", len(data))
	}

	stats := data[0].StatisticValues
	if stats == nil {
		t.Fatal("expected StatisticValues to be set")
	}

	if *stats.SampleCount != 5 {
		t.Errorf("expected count 5, got %f", *stats.SampleCount)
	}
	if *stats.Sum != 150 {
		t.Errorf("expected sum 150, got %f", *stats.Sum)
	}
	if *stats.Minimum != 10 {
		t.Errorf("expected min 10, got %f", *stats.Minimum)
	}
	if *stats.Maximum != 50 {
		t.Errorf("expected max 50, got %f", *stats.Maximum)
	}
}

// TestBatcher_DefaultTags validates default tag inclusion.
func TestBatcher_DefaultTags(t *testing.T) {
	defaultTags := []types.Tag{
		{Key: "service", Value: "bridge"},
		{Key: "env", Value: "production"},
	}
	b := newBatcher("TestNamespace", defaultTags, 100)

	b.add(metricData{
		name:       "test.metric",
		value:      1,
		unit:       cwtypes.StandardUnitCount,
		tags:       []types.Tag{{Key: "topic", Value: "orders"}},
		metricType: metricTypeCounter,
	})

	data := b.drain()
	if len(data) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(data))
	}

	dims := data[0].Dimensions
	if len(dims) != 3 {
		t.Errorf("expected 3 dimensions, got %d", len(dims))
	}

	// Check that all tags are present
	foundTags := make(map[string]string)
	for _, d := range dims {
		foundTags[*d.Name] = *d.Value
	}

	if foundTags["service"] != "bridge" {
		t.Error("expected default tag 'service' to be included")
	}
	if foundTags["env"] != "production" {
		t.Error("expected default tag 'env' to be included")
	}
	if foundTags["topic"] != "orders" {
		t.Error("expected tag 'topic' to be included")
	}
}

// TestBatcher_BufferFull validates buffer full detection.
func TestBatcher_BufferFull(t *testing.T) {
	b := newBatcher("TestNamespace", nil, 5)

	for i := 0; i < 4; i++ {
		full := b.add(metricData{
			name:       "test.metric",
			value:      float64(i),
			metricType: metricTypeCounter,
		})
		if full {
			t.Errorf("expected buffer not full at %d", i)
		}
	}

	full := b.add(metricData{
		name:       "test.metric",
		value:      4,
		metricType: metricTypeCounter,
	})
	if !full {
		t.Error("expected buffer full at 5")
	}
}

// TestBatcher_DrainClearsBuffer validates drain clears buffer.
func TestBatcher_DrainClearsBuffer(t *testing.T) {
	b := newBatcher("TestNamespace", nil, 100)

	b.add(metricData{name: "test", value: 1, metricType: metricTypeCounter})
	b.add(metricData{name: "test.histogram", value: 10, metricType: metricTypeHistogram})

	if b.isEmpty() {
		t.Error("expected buffer not empty before drain")
	}

	b.drain()

	if !b.isEmpty() {
		t.Error("expected buffer empty after drain")
	}
}

// TestConfig_Defaults validates configuration defaults.
func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{Namespace: "Test"}
	applyDefaults(cfg)

	if cfg.FlushInterval != 60*time.Second {
		t.Errorf("expected default FlushInterval 60s, got %v", cfg.FlushInterval)
	}
	if cfg.BufferSize != 1000 {
		t.Errorf("expected default BufferSize 1000, got %d", cfg.BufferSize)
	}
	if cfg.MaxBatchSize != 20 {
		t.Errorf("expected default MaxBatchSize 20, got %d", cfg.MaxBatchSize)
	}
}

// TestOptions validates functional options.
func TestOptions(t *testing.T) {
	e := &Exporter{config: Config{}}

	WithRegion("eu-west-1")(e)
	if e.config.Region != "eu-west-1" {
		t.Errorf("expected region 'eu-west-1', got %s", e.config.Region)
	}

	WithNamespace("MyApp/Metrics")(e)
	if e.config.Namespace != "MyApp/Metrics" {
		t.Errorf("expected namespace 'MyApp/Metrics', got %s", e.config.Namespace)
	}

	WithFlushInterval(30 * time.Second)(e)
	if e.config.FlushInterval != 30*time.Second {
		t.Errorf("expected flush interval 30s, got %v", e.config.FlushInterval)
	}

	WithBufferSize(500)(e)
	if e.config.BufferSize != 500 {
		t.Errorf("expected buffer size 500, got %d", e.config.BufferSize)
	}

	WithEndpoint("http://localhost:4566")(e)
	if e.config.Endpoint != "http://localhost:4566" {
		t.Errorf("expected endpoint 'http://localhost:4566', got %s", e.config.Endpoint)
	}

	tags := []types.Tag{{Key: "env", Value: "test"}}
	WithDefaultTags(tags...)(e)
	if len(e.config.DefaultTags) != 1 {
		t.Errorf("expected 1 default tag, got %d", len(e.config.DefaultTags))
	}
}

// TestBatcher_DimensionLimit validates 30 dimension limit.
func TestBatcher_DimensionLimit(t *testing.T) {
	// Create more than 30 default tags
	var tags []types.Tag
	for i := 0; i < 35; i++ {
		tags = append(tags, types.Tag{Key: "tag" + string(rune('a'+i)), Value: "value"})
	}

	b := newBatcher("TestNamespace", tags, 100)

	b.add(metricData{
		name:       "test.metric",
		value:      1,
		metricType: metricTypeCounter,
	})

	data := b.drain()
	if len(data) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(data))
	}

	// CloudWatch limit is 30 dimensions
	if len(data[0].Dimensions) > 30 {
		t.Errorf("expected max 30 dimensions, got %d", len(data[0].Dimensions))
	}
}
