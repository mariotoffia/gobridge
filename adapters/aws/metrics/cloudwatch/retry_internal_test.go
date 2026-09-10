package cloudwatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// regression: a failed retryable PutMetricData must requeue the drained
// datums so a subsequent flush re-attempts delivery instead of silently
// losing metrics.
func TestFlush_RequeuesOnFailure_ThenDelivers(t *testing.T) {
	b := testBatcher(100)
	b.addCounter("test.counter", 7, []shared.Tag{{Key: "env", Value: "test"}})

	var attempts int
	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("connection reset") // not an APIError => retryable
			}
			return &cloudwatch.PutMetricDataOutput{}, nil
		},
	}

	// First flush fails; datums must be retained for retry.
	if err := b.flush(context.Background(), mock, "Test", 20, 0); err == nil {
		t.Fatal("expected error on first flush")
	}
	if b.isEmpty() {
		t.Fatal("failed flush must retain datums in the retry buffer")
	}

	// Second flush succeeds and delivers the requeued datum.
	if err := b.flush(context.Background(), mock, "Test", 20, 0); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if !b.isEmpty() {
		t.Fatal("successful flush must clear the retry buffer")
	}

	calls := mock.metricDataCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 PutMetricData attempts, got %d", len(calls))
	}
	if len(calls[1].MetricData) != 1 || *calls[1].MetricData[0].MetricName != "test.counter" {
		t.Fatalf("requeued datum not delivered on retry: %+v", calls[1].MetricData)
	}
}

// regression: the retry buffer is bounded; beyond the bound the oldest
// datums are dropped and counted rather than growing without limit.
func TestRequeue_BoundedDropsOldest(t *testing.T) {
	b := newBatcher(Config{
		BufferSize:     100,
		MaxRetryDatums: 2,
		Clock:          clocktest.NewAt(time.Unix(1700000000, 0)),
	})

	mk := func(name string) cwtypes.MetricDatum {
		n := name
		return cwtypes.MetricDatum{MetricName: &n}
	}
	dropped := b.requeue([]cwtypes.MetricDatum{mk("a"), mk("b"), mk("c")})
	if dropped != 1 {
		t.Fatalf("expected 1 dropped, got %d", dropped)
	}
	if got := b.droppedCount(); got != 1 {
		t.Fatalf("droppedCount = %d, want 1", got)
	}
	if len(b.retryBuffer) != 2 {
		t.Fatalf("retry buffer len = %d, want 2 (bounded)", len(b.retryBuffer))
	}
	// Oldest ("a") dropped; newest two retained in order.
	if *b.retryBuffer[0].MetricName != "b" || *b.retryBuffer[1].MetricName != "c" {
		t.Fatalf("unexpected retained datums: %q, %q",
			*b.retryBuffer[0].MetricName, *b.retryBuffer[1].MetricName)
	}
}

// regression: invalid dimensions (empty name/value) are dropped, values are
// truncated to the CloudWatch 256-byte limit, and at most 30 are kept.
func TestBuildDimensions_ValidatesAndBounds(t *testing.T) {
	b := testBatcher(100)

	tags := []shared.Tag{
		{Key: "good", Value: "v"},
		{Key: "", Value: "novalue"},                    // dropped: empty name
		{Key: "noval", Value: ""},                      // dropped: empty value
		{Key: "long", Value: strings.Repeat("x", 300)}, // truncated
	}
	// Add enough valid dimensions to exceed the 30 limit.
	for i := 0; i < 40; i++ {
		tags = append(tags, shared.Tag{Key: "k" + string(rune('A'+i%26)) + string(rune('0'+i/26)), Value: "y"})
	}

	dims := b.buildDimensions(tags, false)
	if len(dims) != maxDimensions {
		t.Fatalf("expected %d dimensions, got %d", maxDimensions, len(dims))
	}
	// First kept dimension is "good"; find the truncated "long".
	for _, d := range dims {
		if *d.Name == "" || *d.Value == "" {
			t.Fatal("empty name/value dimension must be dropped")
		}
		if len(*d.Name) > maxDimensionField || len(*d.Value) > maxDimensionField {
			t.Fatalf("dimension field exceeds %d bytes", maxDimensionField)
		}
	}
}

// regression: a dimension value longer than 256 bytes whose 256th byte
// lands mid-rune must be truncated on a rune boundary, so the produced datum
// is always valid UTF-8. A byte-slice truncation would corrupt the rune and
// CloudWatch would reject the whole all-or-nothing PutMetricData batch.
func TestBuildDimensions_TruncatesOnRuneBoundary(t *testing.T) {
	b := testBatcher(100)

	// 255 ASCII bytes + a 3-byte rune (中) => byte 256 falls INSIDE the rune.
	val := strings.Repeat("a", 255) + "中" + strings.Repeat("b", 50)
	if len(val) <= maxDimensionField {
		t.Fatalf("test value must exceed %d bytes, got %d", maxDimensionField, len(val))
	}

	dims := b.buildDimensions([]shared.Tag{{Key: "k", Value: val}}, false)
	if len(dims) != 1 {
		t.Fatalf("expected 1 dimension, got %d", len(dims))
	}
	got := *dims[0].Value
	if len(got) > maxDimensionField {
		t.Fatalf("value = %d bytes, want <= %d", len(got), maxDimensionField)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated dimension value is not valid UTF-8: %q", got)
	}
	// The straddling rune must be dropped entirely, leaving the 255 ASCII bytes.
	if got != strings.Repeat("a", 255) {
		t.Fatalf("expected rune-boundary truncation to 255 'a's, got %d bytes %q", len(got), got)
	}
}

// regression: a negative MaxRetryDatums must be clamped to the default, not
// treated as "disable the bound" (which would let the retry buffer grow
// without limit since the requeue guard is maxRetry > 0).
func TestApplyDefaults_ClampsNegativeMaxRetryDatums(t *testing.T) {
	cfg := Config{MaxRetryDatums: -1}
	applyDefaults(&cfg)
	if cfg.MaxRetryDatums != 10000 {
		t.Fatalf("MaxRetryDatums = %d, want clamped to 10000", cfg.MaxRetryDatums)
	}
}
