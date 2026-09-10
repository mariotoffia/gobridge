package cloudwatch

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
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

// Exporter self-metric names. The exporter reports its own data
// loss through its own pipeline so silent metric loss is observable on
// the same dashboard as the metrics themselves. Both are published with
// ZERO dimensions (fleet-rollup style) so a single alarm can watch them.
//
// UBIQUITOUS.md has no entry for these yet — they are adapter-local
// self-diagnostics, mirroring the adapter-local dimension-key precedent
// (adapters/aws/transport/sqs/metrics.go TagKeyQueueURL).
const (
	// MetricExporterDroppedDatums counts datums that were ACCEPTED into the
	// export pipeline and then lost: capacity limits (hard buffer cap,
	// retry-buffer overflow) or a non-retryable (validation-class)
	// PutMetricData rejection after the datum was already buffered. Contrast
	// MetricExporterRejectedDatums, which never entered the pipeline.
	MetricExporterDroppedDatums = "ExporterDroppedDatums"
	// MetricExporterRejectedDatums counts emissions REJECTED at add() time
	// because the value was NaN or ±Inf (CloudWatch rejects the whole
	// all-or-nothing batch for such a datum). This is an emit-time
	// rejection: the datum never entered the export pipeline and was never
	// published — contrast MetricExporterDroppedDatums (accepted, then lost).
	// The OTel adapter uses the same name with the same emit-time-rejection
	// semantic (its reason is a full instrument cache instead of NaN/Inf).
	MetricExporterRejectedDatums = "ExporterRejectedDatums"
)

// TagKeyInstanceID is the dimension key used by WithInstanceTag to
// distinguish bridge instances in a fleet (mirrors
// ports.BridgeSettings.InstanceID / UBIQUITOUS.md "BridgeSettings").
// Adapter-local constant, same precedent as sqs.TagKeyQueueURL.
const TagKeyInstanceID = "instance_id"

// metricData is a single buffered gauge sample (counters and
// histograms/timers aggregate into maps instead — see batcher).
type metricData struct {
	name       string
	value      float64
	unit       cwtypes.StandardUnit
	tags       []shared.Tag
	timestamp  time.Time
	metricType metricType
	// rollup marks a zero-dimension fleet-rollup copy of the datum
	// (see WithRollupMetrics). Rollup datums carry NO dimensions —
	// not even DefaultTags — so a dimensionless CloudWatch alarm can
	// match them.
	rollup bool
}

// aggregate accumulates histogram/timer samples per (name, tags) into a
// CloudWatch StatisticSet.
type aggregate struct {
	name   string
	count  float64
	sum    float64
	min    float64
	max    float64
	unit   cwtypes.StandardUnit
	tags   []shared.Tag
	rollup bool
}

// counterAggregate accumulates counter increments per (name, tags)
// within the flush window into one datum: 500 increments/s no
// longer produce 500 datums/s, they produce one summed datum per flush.
type counterAggregate struct {
	name   string
	sum    float64
	unit   cwtypes.StandardUnit
	tags   []shared.Tag
	rollup bool
}

// maxDimensions is the CloudWatch hard limit on dimensions per metric.
const maxDimensions = 30

// maxDimensionField is the CloudWatch hard limit on a dimension name or
// value length (bytes).
const maxDimensionField = 256

// apiMaxBatchDatums is the CloudWatch PutMetricData hard limit on
// datums per request (raised from 20 to 1,000 by AWS in 2022).
const apiMaxBatchDatums = 1000

// apiMaxBatchBytes is the CloudWatch PutMetricData hard limit on total
// request payload size (1 MB).
const apiMaxBatchBytes = 1_000_000

type batcher struct {
	defaultTags []shared.Tag
	// buffer holds individual gauge samples (per-sample timestamps
	// preserve intra-window resolution). Counters and histograms are
	// aggregated in the maps below and never grow per-emission.
	buffer  []metricData
	maxSize int
	// maxBuffered is the HARD cap on pending state (gauge samples +
	// distinct aggregate series). Beyond it new samples/series are
	// dropped and counted: a stalled CloudWatch endpoint must
	// never grow process memory without bound. <= 0 disables the cap
	// (tests only; applyDefaults always sets it for production).
	maxBuffered int
	mu          sync.Mutex
	aggregates  map[string]*aggregate
	counters    map[string]*counterAggregate
	// rollups lists metric names that are double-published without any
	// dimensions so dimensionless alarms match them.
	rollups map[string]struct{}
	clk     clock.Clock
	logger  *slog.Logger

	// retryBuffer holds datums from a failed PutMetricData so the next flush
	// re-attempts delivery instead of silently dropping them. It is bounded
	// by maxRetry; dropped counts are surfaced via droppedTotal.
	retryBuffer []cwtypes.MetricDatum
	maxRetry    int

	// droppedTotal counts datums lost to capacity (buffer cap, retry
	// overflow) or dropped after a non-retryable PutMetricData error.
	// rejectedTotal counts NaN/±Inf emissions rejected at add().
	// The *Reported watermarks track what has already been surfaced as
	// self-metric datums through the pipeline.
	droppedTotal     int64
	rejectedTotal    int64
	droppedReported  int64
	rejectedReported int64

	// dimWarned / capWarned / valWarned latch a single warning per
	// failure category so a hot metric path does not flood the log.
	dimWarned bool
	capWarned bool
	valWarned bool
}

func newBatcher(cfg Config) *batcher {
	rollups := make(map[string]struct{}, len(cfg.RollupMetrics))
	for _, name := range cfg.RollupMetrics {
		rollups[name] = struct{}{}
	}
	return &batcher{
		defaultTags: cfg.DefaultTags,
		buffer:      make([]metricData, 0, cfg.BufferSize),
		maxSize:     cfg.BufferSize,
		maxBuffered: cfg.MaxBufferedDatums,
		aggregates:  make(map[string]*aggregate),
		counters:    make(map[string]*counterAggregate),
		rollups:     rollups,
		clk:         cfg.Clock,
		logger:      cfg.Logger,
		maxRetry:    cfg.MaxRetryDatums,
	}
}

// pendingLocked returns the number of pending series/samples. Callers
// must hold b.mu.
func (b *batcher) pendingLocked() int {
	return len(b.buffer) + len(b.aggregates) + len(b.counters)
}

// add buffers a metric datum. Returns true when the pending state has
// reached the flush-trigger threshold (caller should signal a flush —
// it must NOT flush inline nor spawn a goroutine, see Exporter).
//
// Values that are NaN or ±Inf are rejected here: CloudWatch
// rejects them with InvalidParameterValue, and since PutMetricData is
// all-or-nothing one poison datum would fail whole batches forever.
func (b *batcher) add(md metricData) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if math.IsNaN(md.value) || math.IsInf(md.value, 0) {
		b.rejectedTotal++
		if b.logger != nil && !b.valWarned {
			b.valWarned = true
			b.logger.Warn("cloudwatch: rejected metric with NaN/Inf value (further rejects counted, not logged)",
				slog.String("metric", md.name))
		}
		return false
	}

	b.insertLocked(md)

	if _, ok := b.rollups[md.name]; ok {
		rc := md
		rc.tags = nil
		rc.rollup = true
		b.insertLocked(rc)
	}

	return b.pendingLocked() >= b.maxSize
}

// insertLocked routes one validated datum into the right structure.
// Callers must hold b.mu.
func (b *batcher) insertLocked(md metricData) {
	switch md.metricType {
	case metricTypeCounter:
		key := seriesKey(md.name, md.tags, md.rollup)
		if agg, ok := b.counters[key]; ok {
			agg.sum += md.value
			return
		}
		if b.capacityExhaustedLocked() {
			b.dropForCapacityLocked(md.name)
			return
		}
		b.counters[key] = &counterAggregate{
			name:   md.name,
			sum:    md.value,
			unit:   md.unit,
			tags:   md.tags,
			rollup: md.rollup,
		}

	case metricTypeHistogram:
		key := seriesKey(md.name, md.tags, md.rollup)
		if agg, ok := b.aggregates[key]; ok {
			agg.count++
			agg.sum += md.value
			if md.value < agg.min {
				agg.min = md.value
			}
			if md.value > agg.max {
				agg.max = md.value
			}
			return
		}
		if b.capacityExhaustedLocked() {
			b.dropForCapacityLocked(md.name)
			return
		}
		b.aggregates[key] = &aggregate{
			name:   md.name,
			count:  1,
			sum:    md.value,
			min:    md.value,
			max:    md.value,
			unit:   md.unit,
			tags:   md.tags,
			rollup: md.rollup,
		}

	case metricTypeGauge:
		if b.capacityExhaustedLocked() {
			b.dropForCapacityLocked(md.name)
			return
		}
		md.timestamp = b.clk.Now()
		b.buffer = append(b.buffer, md)
	}
}

// capacityExhaustedLocked reports whether the hard buffer cap has been
// reached. Existing aggregate series keep accumulating past the cap
// (bounded memory, no data loss for known series); only NEW
// samples/series are dropped. Callers must hold b.mu.
func (b *batcher) capacityExhaustedLocked() bool {
	return b.maxBuffered > 0 && b.pendingLocked() >= b.maxBuffered
}

func (b *batcher) dropForCapacityLocked(name string) {
	b.droppedTotal++
	if b.logger != nil && !b.capWarned {
		b.capWarned = true
		b.logger.Warn("cloudwatch: metric buffer hard cap reached; dropping new samples (further drops counted, not logged)",
			slog.String("metric", name),
			slog.Int("max_buffered", b.maxBuffered))
	}
}

// drain removes and converts all buffered metrics to CloudWatch format,
// prepending any datums requeued from a previous failed flush so they are
// re-attempted ahead of fresh samples. It also appends self-metric datums
// for any drops/rejects not yet reported.
func (b *batcher) drain() []cwtypes.MetricDatum {
	b.mu.Lock()
	defer b.mu.Unlock()

	var result []cwtypes.MetricDatum
	if len(b.retryBuffer) > 0 {
		result = append(result, b.retryBuffer...)
		b.retryBuffer = nil
	}

	for _, md := range b.buffer {
		name := md.name
		val := md.value
		ts := md.timestamp
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			Value:      &val,
			Unit:       md.unit,
			Timestamp:  &ts,
			Dimensions: b.buildDimensions(md.tags, md.rollup),
		})
	}

	now := b.clk.Now()
	for _, agg := range b.counters {
		name := agg.name
		val := agg.sum
		ts := now
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			Value:      &val,
			Unit:       agg.unit,
			Timestamp:  &ts,
			Dimensions: b.buildDimensions(agg.tags, agg.rollup),
		})
	}
	for _, agg := range b.aggregates {
		name := agg.name
		count := agg.count
		sum := agg.sum
		minV := agg.min
		maxV := agg.max
		ts := now
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			StatisticValues: &cwtypes.StatisticSet{
				SampleCount: &count,
				Sum:         &sum,
				Minimum:     &minV,
				Maximum:     &maxV,
			},
			Unit:       agg.unit,
			Timestamp:  &ts,
			Dimensions: b.buildDimensions(agg.tags, agg.rollup),
		})
	}

	result = append(result, b.selfMetricsLocked(now)...)

	b.buffer = make([]metricData, 0, b.maxSize)
	b.aggregates = make(map[string]*aggregate)
	b.counters = make(map[string]*counterAggregate)
	return result
}

// selfMetricsLocked builds zero-dimension datums reporting drops and
// rejects accumulated since the last report. Callers must hold
// b.mu.
func (b *batcher) selfMetricsLocked(now time.Time) []cwtypes.MetricDatum {
	var result []cwtypes.MetricDatum
	if delta := b.droppedTotal - b.droppedReported; delta > 0 {
		name := MetricExporterDroppedDatums
		val := float64(delta)
		ts := now
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			Value:      &val,
			Unit:       cwtypes.StandardUnitCount,
			Timestamp:  &ts,
		})
		b.droppedReported = b.droppedTotal
	}
	if delta := b.rejectedTotal - b.rejectedReported; delta > 0 {
		name := MetricExporterRejectedDatums
		val := float64(delta)
		ts := now
		result = append(result, cwtypes.MetricDatum{
			MetricName: &name,
			Value:      &val,
			Unit:       cwtypes.StandardUnitCount,
			Timestamp:  &ts,
		})
		b.rejectedReported = b.rejectedTotal
	}
	return result
}

func (b *batcher) isEmpty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingLocked() == 0 && len(b.retryBuffer) == 0 &&
		b.droppedTotal == b.droppedReported && b.rejectedTotal == b.rejectedReported
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

// recordDropped counts datums dropped after a non-retryable
// PutMetricData rejection (classification).
func (b *batcher) recordDropped(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.droppedTotal += n
}

func (b *batcher) droppedCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.droppedTotal
}

func (b *batcher) rejectedCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rejectedTotal
}

// buildDimensions converts tags to CloudWatch dimensions, enforcing the
// CloudWatch limits: dimensions with an empty name or value are dropped
// (CloudWatch rejects them, which would fail the whole PutMetricData batch),
// name/value are truncated to 256 bytes, and at most 30 dimensions are kept.
// Excess/invalid dimensions are dropped with a single latched warning rather
// than silently discarded, since callers are cautioned against high-cardinality
// dimensions (see docs/aws-deployment/monitoring.md).
//
// rollup datums get NO dimensions at all — not even DefaultTags — so a
// dimensionless alarm matches them and fleet instances aggregate into
// one series by design.
func (b *batcher) buildDimensions(tags []shared.Tag, rollup bool) []cwtypes.Dimension {
	if rollup {
		return nil
	}
	// slices.Concat allocates a fresh backing array, so the returned slice
	// never aliases b.defaultTags' spare capacity — a plain
	// append(b.defaultTags, tags...) could write into shared capacity and
	// bleed dimensions across concurrent datums on a future refactor.
	all := slices.Concat(b.defaultTags, tags)
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
// entire batch.
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

// seriesKey builds an unambiguous aggregation map key from a metric
// name, its tags, and the rollup marker. Every component is
// length-prefixed: the old "|"-joined form folded distinct
// series whose concatenations collided (e.g. tag value containing "|")
// and truncated names containing the separator. The metric name and
// tags are stored on the aggregate structs — the key is never decoded.
func seriesKey(name string, tags []shared.Tag, rollup bool) string {
	var sb strings.Builder
	if rollup {
		sb.WriteByte('r')
	} else {
		sb.WriteByte('p')
	}
	appendKeyComponent(&sb, name)
	for _, t := range tags {
		appendKeyComponent(&sb, t.Key)
		appendKeyComponent(&sb, t.Value)
	}
	return sb.String()
}

func appendKeyComponent(sb *strings.Builder, s string) {
	sb.WriteString(strconv.Itoa(len(s)))
	sb.WriteByte(':')
	sb.WriteString(s)
}

// estimateDatumSize conservatively estimates the serialized request
// size contribution of one datum so batches stay under the CloudWatch
// 1 MB PutMetricData payload limit.
func estimateDatumSize(d cwtypes.MetricDatum) int {
	size := 160 // field names, timestamp, value/statistic-set overhead
	if d.MetricName != nil {
		size += len(*d.MetricName)
	}
	for _, dim := range d.Dimensions {
		size += 64
		if dim.Name != nil {
			size += len(*dim.Name)
		}
		if dim.Value != nil {
			size += len(*dim.Value)
		}
	}
	return size
}

// addCounter buffers a counter sample. Increments aggregate per
// (name, tags) within the flush window. Returns true when the
// pending state has reached the flush-trigger threshold.
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
// client. Sends are chunked by both datum count and estimated payload size
// (the CloudWatch PutMetricData hard limits). A no-op when the batcher is
// empty.
//
// Error classification: a non-retryable (validation-class 4xx)
// rejection drops ONLY the offending batch — counted and logged — and
// sending continues, so one poison datum can no longer black out the whole
// pipeline. Retryable errors (throttling, 5xx, network) requeue everything
// not yet delivered, bounded by maxRetry.
func (b *batcher) flush(ctx context.Context, client cloudWatchAPI, namespace string, maxBatchSize, maxBatchBytes int) error {
	if b.isEmpty() {
		return nil
	}
	data := b.drain()
	if len(data) == 0 {
		return nil
	}
	if maxBatchSize <= 0 || maxBatchSize > apiMaxBatchDatums {
		maxBatchSize = apiMaxBatchDatums
	}
	if maxBatchBytes <= 0 || maxBatchBytes > apiMaxBatchBytes {
		maxBatchBytes = apiMaxBatchBytes
	}

	var permErr error
	var permDropped int64
	i := 0
	for i < len(data) {
		end := i
		size := 0
		for end < len(data) && end-i < maxBatchSize {
			ds := estimateDatumSize(data[end])
			if end > i && size+ds > maxBatchBytes {
				break
			}
			size += ds
			end++
		}

		_, err := client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(namespace),
			MetricData: data[i:end],
		})
		switch {
		case err == nil:

		case isPermanentPutError(err):
			n := int64(end - i)
			b.recordDropped(n)
			permDropped += n
			permErr = err
			if b.logger != nil {
				b.logger.Warn("cloudwatch: dropped metric batch rejected with non-retryable error",
					slog.Int64("dropped", n),
					slog.String("error", err.Error()),
				)
			}

		default:
			// Retryable: requeue everything not yet delivered (this batch
			// and the remainder) so the next flush re-attempts it instead
			// of losing the samples that were drained out of the buffer.
			if dropped := b.requeue(data[i:]); dropped > 0 && b.logger != nil {
				b.logger.Warn("cloudwatch: dropped requeued metric datums (retry buffer full)",
					slog.Int64("dropped", dropped),
					slog.Int64("dropped_total", b.droppedCount()),
				)
			}
			return fmt.Errorf("cloudwatch: put metric data: %w", err)
		}
		i = end
	}
	if permErr != nil {
		return fmt.Errorf("cloudwatch: put metric data: dropped %d datums rejected as invalid: %w", permDropped, permErr)
	}
	return nil
}
