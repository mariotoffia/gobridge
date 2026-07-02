package cloudwatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type metricType int

const (
	metricTypeCounter metricType = iota
	metricTypeGauge
	metricTypeHistogram
)

type metricData struct {
	name       string
	value      float64
	unit       cwtypes.StandardUnit
	tags       []shared.Tag
	timestamp  time.Time
	metricType metricType
}

type aggregate struct {
	count float64
	sum   float64
	min   float64
	max   float64
	unit  cwtypes.StandardUnit
	tags  []shared.Tag
}

// maxDimensions is the CloudWatch hard limit on dimensions per metric.
const maxDimensions = 30

// maxDimensionField is the CloudWatch hard limit on a dimension name or
// value length (bytes).
const maxDimensionField = 256

type batcher struct {
	namespace   string
	defaultTags []shared.Tag
	buffer      []metricData
	maxSize     int
	mu          sync.Mutex
	aggregates  map[string]*aggregate
	clk         clock.Clock
	logger      *slog.Logger

	// retryBuffer holds datums from a failed PutMetricData so the next flush
	// re-attempts delivery instead of silently dropping them. It is bounded
	// by maxRetry; dropped counts are surfaced via droppedTotal.
	retryBuffer  []cwtypes.MetricDatum
	maxRetry     int
	droppedTotal int64

	// dimWarned latches a single high-cardinality/over-limit dimension
	// warning so a hot metric path does not flood the log.
	dimWarned bool
}

func newBatcher(namespace string, defaultTags []shared.Tag, maxSize int, clk clock.Clock, logger *slog.Logger, maxRetry int) *batcher {
	return &batcher{
		namespace:   namespace,
		defaultTags: defaultTags,
		buffer:      make([]metricData, 0, maxSize),
		maxSize:     maxSize,
		aggregates:  make(map[string]*aggregate),
		clk:         clk,
		logger:      logger,
		maxRetry:    maxRetry,
	}
}

// add buffers a metric datum. Returns true when the non-histogram buffer is full.
func (b *batcher) add(md metricData) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if md.metricType == metricTypeHistogram {
		key := aggregateKey(md.name, md.tags)
		if agg, ok := b.aggregates[key]; ok {
			agg.count++
			agg.sum += md.value
			if md.value < agg.min {
				agg.min = md.value
			}
			if md.value > agg.max {
				agg.max = md.value
			}
		} else {
			b.aggregates[key] = &aggregate{
				count: 1,
				sum:   md.value,
				min:   md.value,
				max:   md.value,
				unit:  md.unit,
				tags:  md.tags,
			}
		}
		return len(b.buffer) >= b.maxSize
	}

	md.timestamp = b.clk.Now()
	b.buffer = append(b.buffer, md)
	return len(b.buffer) >= b.maxSize
}

// drain removes and converts all buffered metrics to CloudWatch format,
// prepending any datums requeued from a previous failed flush so they are
// re-attempted ahead of fresh samples.
func (b *batcher) drain() []cwtypes.MetricDatum {
	b.mu.Lock()
	defer b.mu.Unlock()

	var result []cwtypes.MetricDatum
	if len(b.retryBuffer) > 0 {
		result = append(result, b.retryBuffer...)
		b.retryBuffer = nil
	}

	for _, md := range b.buffer {
		dims := b.buildDimensions(md.tags)
		name := md.name
		val := md.value
		ts := md.timestamp
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			Value:      &val,
			Unit:       md.unit,
			Timestamp:  &ts,
			Dimensions: dims,
		})
	}

	now := b.clk.Now()
	for key, agg := range b.aggregates {
		name := metricNameFromKey(key)
		dims := b.buildDimensions(agg.tags)
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			StatisticValues: &cwtypes.StatisticSet{
				SampleCount: &agg.count,
				Sum:         &agg.sum,
				Minimum:     &agg.min,
				Maximum:     &agg.max,
			},
			Unit:       agg.unit,
			Timestamp:  &now,
			Dimensions: dims,
		})
	}

	b.buffer = make([]metricData, 0, b.maxSize)
	b.aggregates = make(map[string]*aggregate)
	return result
}

func (b *batcher) isEmpty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer) == 0 && len(b.aggregates) == 0 && len(b.retryBuffer) == 0
}

// requeue stores datums from a failed PutMetricData for the next flush,
// bounded by maxRetry. When the bound is exceeded the oldest datums are
// dropped and counted so a persistently failing endpoint cannot leak memory.
// Returns the number of datums dropped by this call.
func (b *batcher) requeue(datums []cwtypes.MetricDatum) int64 {
	if len(datums) == 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.retryBuffer = append(b.retryBuffer, datums...)
	var dropped int64
	if b.maxRetry > 0 && len(b.retryBuffer) > b.maxRetry {
		dropped = int64(len(b.retryBuffer) - b.maxRetry)
		// Drop the oldest datums, keep the most recent maxRetry.
		b.retryBuffer = b.retryBuffer[len(b.retryBuffer)-b.maxRetry:]
		b.droppedTotal += dropped
	}
	return dropped
}

func (b *batcher) droppedCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.droppedTotal
}

// buildDimensions converts tags to CloudWatch dimensions, enforcing the
// CloudWatch limits: dimensions with an empty name or value are dropped
// (CloudWatch rejects them, which would fail the whole PutMetricData batch),
// name/value are truncated to 256 bytes, and at most 30 dimensions are kept.
// Excess/invalid dimensions are dropped with a single latched warning rather
// than silently discarded, since callers are cautioned against high-cardinality
// dimensions (see docs/aws-deployment/monitoring.md).
func (b *batcher) buildDimensions(tags []shared.Tag) []cwtypes.Dimension {
	all := append(b.defaultTags, tags...)
	if len(all) == 0 {
		return nil
	}
	dims := make([]cwtypes.Dimension, 0, len(all))
	var invalid, excess int
	for _, tag := range all {
		if tag.Key == "" || tag.Value == "" {
			invalid++
			continue
		}
		if len(dims) >= maxDimensions {
			excess++
			continue
		}
		k := truncateField(tag.Key)
		v := truncateField(tag.Value)
		dims = append(dims, cwtypes.Dimension{Name: &k, Value: &v})
	}
	if (invalid > 0 || excess > 0) && b.logger != nil && !b.dimWarned {
		b.dimWarned = true
		b.logger.Warn("cloudwatch: dropped invalid or excess metric dimensions",
			slog.Int("invalid", invalid),
			slog.Int("excess", excess),
			slog.Int("limit", maxDimensions),
		)
	}
	if len(dims) == 0 {
		return nil
	}
	return dims
}

// truncateField caps a dimension name/value at the CloudWatch 256-byte limit
// on a UTF-8 rune boundary. A naive byte slice can split a multi-byte rune,
// producing invalid UTF-8 that CloudWatch rejects with InvalidParameterValue
// — and since PutMetricData is all-or-nothing, one poison datum fails the
// entire batch (MF-2/J10).
//
// Operator caution (aggregation collision): truncation is lossy, and
// CloudWatch identifies a metric series by its full set of dimension
// name=value pairs. Two dimension values that share the same first 256
// bytes truncate to the SAME string here, so CloudWatch folds their datums
// into ONE series — silently aggregating what were meant to be distinct
// timeseries (the same holds for two colliding dimension names). This is
// inherent to the CloudWatch maxDimensionField limit, not a bug in
// buildDimensions. Keep high-cardinality dimension values distinguishable
// WITHIN their first 256 bytes (put the varying part — e.g. an id — early,
// not after a long shared prefix).
func truncateField(s string) string {
	if len(s) <= maxDimensionField {
		return s
	}
	b := s[:maxDimensionField]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

func aggregateKey(name string, tags []shared.Tag) string {
	key := name
	for _, t := range tags {
		key += "|" + t.Key + "=" + t.Value
	}
	return key
}

func metricNameFromKey(key string) string {
	for i, c := range key {
		if c == '|' {
			return key[:i]
		}
	}
	return key
}

// addCounter buffers a counter sample. Returns true when the non-histogram
// buffer has reached its configured capacity (caller should trigger a flush).
func (b *batcher) addCounter(name string, value int64, tags []shared.Tag) bool {
	return b.add(metricData{
		name:       name,
		value:      float64(value),
		unit:       cwtypes.StandardUnitCount,
		tags:       tags,
		metricType: metricTypeCounter,
	})
}

// addGauge buffers a gauge sample.
func (b *batcher) addGauge(name string, value float64, tags []shared.Tag) bool {
	return b.add(metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeGauge,
	})
}

// addHistogram buffers a histogram sample. Histogram samples aggregate into
// CloudWatch StatisticSets on drain.
func (b *batcher) addHistogram(name string, value float64, tags []shared.Tag) bool {
	return b.add(metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeHistogram,
	})
}

// addTimer buffers a duration sample (treated as a histogram in milliseconds).
func (b *batcher) addTimer(name string, duration time.Duration, tags []shared.Tag) bool {
	ms := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	return b.add(metricData{
		name:       name,
		value:      ms,
		unit:       cwtypes.StandardUnitMilliseconds,
		tags:       tags,
		metricType: metricTypeHistogram,
	})
}

// flush drains buffered samples and sends them to CloudWatch via the supplied
// client. Sends are chunked into batches of at most maxBatchSize datums, the
// CloudWatch PutMetricData hard limit. A no-op when the batcher is empty.
func (b *batcher) flush(ctx context.Context, client cloudWatchAPI, namespace string, maxBatchSize int) error {
	if b.isEmpty() {
		return nil
	}
	data := b.drain()
	if len(data) == 0 {
		return nil
	}
	if maxBatchSize <= 0 {
		maxBatchSize = len(data)
	}
	for i := 0; i < len(data); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(data) {
			end = len(data)
		}
		_, err := client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(namespace),
			MetricData: data[i:end],
		})
		if err != nil {
			// Requeue everything not yet delivered (this batch and the
			// remainder) so the next flush re-attempts it instead of losing
			// the samples that were drained out of the buffer.
			if dropped := b.requeue(data[i:]); dropped > 0 && b.logger != nil {
				b.logger.Warn("cloudwatch: dropped requeued metric datums (retry buffer full)",
					slog.Int64("dropped", dropped),
					slog.Int64("dropped_total", b.droppedCount()),
				)
			}
			return fmt.Errorf("cloudwatch: put metric data: %w", err)
		}
	}
	return nil
}
