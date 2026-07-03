package cloudwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Verifies New with a mock client creates a working exporter.
func TestNew_WithMockClient(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	if e.config.Namespace != "Test/NS" {
		t.Errorf("Namespace = %q, want Test/NS", e.config.Namespace)
	}
}

// Verifies Flush sends buffered counter metrics via PutMetricData.
func TestExporter_Flush_SendsCounters(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Counter("requests", 5, shared.Tag{Key: "route", Value: "orders"})
	e.Counter("errors", 1)

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := mock.metricDataCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutMetricData call, got %d", len(calls))
	}
	if len(calls[0].MetricData) != 2 {
		t.Errorf("expected 2 datums, got %d", len(calls[0].MetricData))
	}
	if *calls[0].Namespace != "Test/NS" {
		t.Errorf("Namespace = %q, want Test/NS", *calls[0].Namespace)
	}
}

// Verifies Flush sends histogram aggregates via PutMetricData.
func TestExporter_Flush_SendsHistograms(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Histogram("latency", 10)
	e.Histogram("latency", 20)
	e.Histogram("latency", 30)

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := mock.metricDataCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutMetricData call, got %d", len(calls))
	}
	if len(calls[0].MetricData) != 1 {
		t.Fatalf("expected 1 aggregated datum, got %d", len(calls[0].MetricData))
	}

	sv := calls[0].MetricData[0].StatisticValues
	if sv == nil {
		t.Fatal("expected StatisticValues")
	}
	if *sv.SampleCount != 3 {
		t.Errorf("SampleCount = %f, want 3", *sv.SampleCount)
	}
	if *sv.Sum != 60 {
		t.Errorf("Sum = %f, want 60", *sv.Sum)
	}
}

// Verifies Flush sends timer metrics as millisecond histograms.
func TestExporter_Flush_SendsTimers(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Timer("response_time", 150*time.Millisecond)

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := mock.metricDataCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutMetricData call, got %d", len(calls))
	}

	sv := calls[0].MetricData[0].StatisticValues
	if sv == nil {
		t.Fatal("expected StatisticValues for timer")
	}
	if *sv.Sum < 149 || *sv.Sum > 151 {
		t.Errorf("Sum = %f, want ~150 ms", *sv.Sum)
	}
}

// Verifies Flush is a no-op when the buffer is empty.
func TestExporter_Flush_EmptyBuffer(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(mock.metricDataCalls()) != 0 {
		t.Error("expected no PutMetricData calls for empty buffer")
	}
}

// Verifies Flush propagates PutMetricData errors.
func TestExporter_Flush_APIError(t *testing.T) {
	apiErr := errors.New("throttled")
	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			return nil, apiErr
		},
	}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Counter("m", 1)
	err = e.Flush(ctx)
	if err == nil {
		t.Fatal("expected error from Flush")
	}
	if !errors.Is(err, apiErr) {
		t.Errorf("expected wrapped apiErr, got: %v", err)
	}
}

// Verifies flush splits data into batches of MaxBatchSize datums.
// Counters aggregate per (name, tags) (MF-6), so distinct gauge samples
// are used to produce many datums.
func TestExporter_SendBatched_Splits(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
		WithBufferSize(100),
		WithMaxBatchSize(20),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	for i := 0; i < 45; i++ {
		e.Gauge("m", float64(i))
	}

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := mock.metricDataCalls()
	// 45 datums / 20 per batch = 3 calls (20, 20, 5)
	if len(calls) != 3 {
		t.Fatalf("expected 3 PutMetricData calls, got %d", len(calls))
	}
	if len(calls[0].MetricData) != 20 {
		t.Errorf("batch 0: expected 20 datums, got %d", len(calls[0].MetricData))
	}
	if len(calls[1].MetricData) != 20 {
		t.Errorf("batch 1: expected 20 datums, got %d", len(calls[1].MetricData))
	}
	if len(calls[2].MetricData) != 5 {
		t.Errorf("batch 2: expected 5 datums, got %d", len(calls[2].MetricData))
	}
}

// Verifies Close performs a final flush and is safe to call multiple times.
func TestExporter_Close_FlushesAndIdempotent(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.Counter("m", 1)

	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	calls := mock.metricDataCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutMetricData call after Close, got %d", len(calls))
	}

	// Second close should be safe.
	if err := e.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(mock.metricDataCalls()) != 1 {
		t.Error("expected no additional calls after second Close")
	}
}

// Verifies Gauge metrics are sent with correct value.
func TestExporter_Flush_SendsGauges(t *testing.T) {
	mock := &mockCloudWatch{}
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Gauge("cpu_usage", 72.5, shared.Tag{Key: "host", Value: "prod-1"})

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := mock.metricDataCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if len(calls[0].MetricData) != 1 {
		t.Fatalf("expected 1 datum, got %d", len(calls[0].MetricData))
	}
	if *calls[0].MetricData[0].Value != 72.5 {
		t.Errorf("value = %f, want 72.5", *calls[0].MetricData[0].Value)
	}
	if *calls[0].MetricData[0].MetricName != "cpu_usage" {
		t.Errorf("name = %q, want cpu_usage", *calls[0].MetricData[0].MetricName)
	}
}
