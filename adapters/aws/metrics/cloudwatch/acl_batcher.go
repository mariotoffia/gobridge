package cloudwatch

import (
	"context"
	"fmt"
	"sync"
	"time"

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

type batcher struct {
	namespace   string
	defaultTags []shared.Tag
	buffer      []metricData
	maxSize     int
	mu          sync.Mutex
	aggregates  map[string]*aggregate
	clk         clock.Clock
}

func newBatcher(namespace string, defaultTags []shared.Tag, maxSize int, clk clock.Clock) *batcher {
	return &batcher{
		namespace:   namespace,
		defaultTags: defaultTags,
		buffer:      make([]metricData, 0, maxSize),
		maxSize:     maxSize,
		aggregates:  make(map[string]*aggregate),
		clk:         clk,
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

// drain removes and converts all buffered metrics to CloudWatch format.
func (b *batcher) drain() []cwtypes.MetricDatum {
	b.mu.Lock()
	defer b.mu.Unlock()

	var result []cwtypes.MetricDatum

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
	return len(b.buffer) == 0 && len(b.aggregates) == 0
}

func (b *batcher) buildDimensions(tags []shared.Tag) []cwtypes.Dimension {
	all := append(b.defaultTags, tags...)
	if len(all) == 0 {
		return nil
	}
	if len(all) > 30 {
		all = all[:30]
	}
	dims := make([]cwtypes.Dimension, len(all))
	for i, tag := range all {
		k := tag.Key
		v := tag.Value
		dims[i] = cwtypes.Dimension{Name: &k, Value: &v}
	}
	return dims
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
			return fmt.Errorf("cloudwatch: put metric data: %w", err)
		}
	}
	return nil
}
