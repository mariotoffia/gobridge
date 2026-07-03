package cloudwatch

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MF-2: NaN and ±Inf values are rejected at add() — CloudWatch rejects
// them with InvalidParameterValue and PutMetricData is all-or-nothing,
// so one poison datum would otherwise fail whole batches forever.
func TestBatcher_RejectsNaNAndInf(t *testing.T) {
	b := testBatcher(100)

	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if full := b.addGauge("poison", v, nil); full {
			t.Error("rejected value must not trigger a flush")
		}
	}
	if got := b.rejectedCount(); got != 3 {
		t.Fatalf("rejectedCount = %d, want 3", got)
	}
	if b.isEmpty() {
		t.Fatal("pending self-metric report must keep the batcher non-empty")
	}

	data := b.drain()
	// No poison datums, only the ExporterRejectedDatums self-metric.
	if len(data) != 1 {
		t.Fatalf("expected only the self-metric datum, got %d", len(data))
	}
	if *data[0].MetricName != MetricExporterRejectedDatums {
		t.Errorf("name = %q, want %s", *data[0].MetricName, MetricExporterRejectedDatums)
	}
	if *data[0].Value != 3 {
		t.Errorf("value = %f, want 3", *data[0].Value)
	}
	if len(data[0].Dimensions) != 0 {
		t.Errorf("self-metric must have zero dimensions, got %d", len(data[0].Dimensions))
	}
}

// MF-1: the hard buffer cap drops new samples (counted) instead of
// growing memory without bound while flushing is stalled.
func TestBatcher_HardCapDropsAndCounts(t *testing.T) {
	b := newBatcher(Config{
		BufferSize:        1000,
		MaxBufferedDatums: 3,
		Clock:             clocktest.NewAt(time.Unix(1700000000, 0)),
	})

	for i := 0; i < 10; i++ {
		b.addGauge("g", float64(i), []shared.Tag{{Key: "i", Value: string(rune('a' + i))}})
	}
	if got := b.droppedCount(); got != 7 {
		t.Fatalf("droppedCount = %d, want 7", got)
	}

	data := b.drain()
	// 3 retained gauges + 1 ExporterDroppedDatums self-metric.
	if len(data) != 4 {
		t.Fatalf("expected 4 datums (3 gauges + self-metric), got %d", len(data))
	}
	var self float64 = -1
	for _, d := range data {
		if *d.MetricName == MetricExporterDroppedDatums {
			self = *d.Value
		}
	}
	if self != 7 {
		t.Errorf("ExporterDroppedDatums = %f, want 7", self)
	}
}

// MF-1: existing aggregate series keep accumulating past the hard cap —
// only NEW series/samples are dropped, so no counts are lost for series
// already being tracked.
func TestBatcher_HardCapKeepsAggregatingExistingSeries(t *testing.T) {
	b := newBatcher(Config{
		BufferSize:        1000,
		MaxBufferedDatums: 1,
		Clock:             clocktest.NewAt(time.Unix(1700000000, 0)),
	})

	b.addCounter("c", 1, nil)
	b.addCounter("c", 1, nil) // same series: accumulates past the cap
	b.addCounter("other", 1, nil)
	if got := b.droppedCount(); got != 1 {
		t.Fatalf("droppedCount = %d, want 1 (only the new series)", got)
	}

	data := b.drain()
	for _, d := range data {
		if *d.MetricName == "c" && *d.Value != 2 {
			t.Errorf("existing series sum = %f, want 2", *d.Value)
		}
	}
}

// MF-5: drop/reject totals are reported once per flush window through
// the exporter's own pipeline, as deltas.
func TestBatcher_SelfMetricsReportDeltasOnce(t *testing.T) {
	b := testBatcher(100)
	b.addGauge("nan", math.NaN(), nil)

	first := b.drain()
	if len(first) != 1 || *first[0].MetricName != MetricExporterRejectedDatums {
		t.Fatalf("expected one self-metric datum, got %+v", first)
	}

	// Nothing new: nothing to report.
	if !b.isEmpty() {
		t.Fatal("batcher must be empty after self-metric was reported")
	}
	if second := b.drain(); len(second) != 0 {
		t.Fatalf("expected no repeated self-metric, got %d datums", len(second))
	}
}

// smithyErr is a minimal smithy.APIError test double.
type smithyErr struct {
	code  string
	fault smithy.ErrorFault
}

func (e *smithyErr) Error() string                 { return e.code }
func (e *smithyErr) ErrorCode() string             { return e.code }
func (e *smithyErr) ErrorMessage() string          { return e.code }
func (e *smithyErr) ErrorFault() smithy.ErrorFault { return e.fault }

// MF-3: classification table for PutMetricData failures.
func TestIsPermanentPutError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"validation client fault", &smithyErr{code: "InvalidParameterValue", fault: smithy.FaultClient}, true},
		{"missing parameter", &smithyErr{code: "MissingRequiredParameter", fault: smithy.FaultClient}, true},
		{"throttling is retryable", &smithyErr{code: "Throttling", fault: smithy.FaultClient}, false},
		{"throttling exception is retryable", &smithyErr{code: "ThrottlingException", fault: smithy.FaultClient}, false},
		{"server fault is retryable", &smithyErr{code: "InternalServiceError", fault: smithy.FaultServer}, false},
		{"unknown fault is retryable", &smithyErr{code: "Weird", fault: smithy.FaultUnknown}, false},
		{"network error is retryable", errors.New("connection refused"), false},
		{"wrapped validation is permanent", wrapErr{&smithyErr{code: "InvalidParameterValue", fault: smithy.FaultClient}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanentPutError(tt.err); got != tt.want {
				t.Errorf("isPermanentPutError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// wrapErr wraps an error so errors.As traversal is exercised.
type wrapErr struct{ inner error }

func (w wrapErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrapErr) Unwrap() error { return w.inner }

// MF-3 regression: a validation-class rejection drops ONLY the offending
// batch — counted, not requeued — so a poison datum cannot black out the
// pipeline by being requeued and failing every subsequent flush.
func TestFlush_PermanentErrorDropsBatchNotRequeued(t *testing.T) {
	b := testBatcher(100)
	b.addCounter("poison.batch", 1, nil)

	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			return nil, &smithyErr{code: "InvalidParameterValue", fault: smithy.FaultClient}
		},
	}

	err := b.flush(context.Background(), mock, "Test", 20, 0)
	if err == nil {
		t.Fatal("expected error surfaced from permanent rejection")
	}
	if len(b.retryBuffer) != 0 {
		t.Fatalf("permanent rejection must NOT requeue, retry buffer len = %d", len(b.retryBuffer))
	}
	if got := b.droppedCount(); got != 1 {
		t.Fatalf("droppedCount = %d, want 1", got)
	}

	// Next flush reports the drop as a self-metric and succeeds.
	mock.PutMetricDataFn = nil
	if err := b.flush(context.Background(), mock, "Test", 20, 0); err != nil {
		t.Fatalf("follow-up flush: %v", err)
	}
	calls := mock.metricDataCalls()
	last := calls[len(calls)-1]
	found := false
	for _, d := range last.MetricData {
		if *d.MetricName == MetricExporterDroppedDatums && *d.Value == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ExporterDroppedDatums self-metric in follow-up flush, got %+v", last.MetricData)
	}
}

// MF-3: a permanent rejection of one batch must not stop delivery of the
// remaining batches in the same flush.
func TestFlush_PermanentErrorContinuesWithRemainingBatches(t *testing.T) {
	b := testBatcher(1000)
	for i := 0; i < 3; i++ {
		b.addGauge("g", float64(i), []shared.Tag{{Key: "i", Value: string(rune('a' + i))}})
	}

	var call int
	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			call++
			if call == 1 {
				return nil, &smithyErr{code: "InvalidParameterValue", fault: smithy.FaultClient}
			}
			return &cloudwatch.PutMetricDataOutput{}, nil
		},
	}

	// Batch size 1 => 3 batches; first is rejected permanently, the
	// remaining two must still be delivered.
	err := b.flush(context.Background(), mock, "Test", 1, 0)
	if err == nil {
		t.Fatal("expected surfaced permanent error")
	}
	if got := mock.metricDataCalls(); len(got) != 3 {
		t.Fatalf("expected 3 PutMetricData calls, got %d", len(got))
	}
	if got := b.droppedCount(); got != 1 {
		t.Fatalf("droppedCount = %d, want 1", got)
	}
	if len(b.retryBuffer) != 0 {
		t.Fatalf("nothing should be requeued, retry buffer len = %d", len(b.retryBuffer))
	}
}
