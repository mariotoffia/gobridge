package cloudwatch

import (
	"sync"
	"time"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// metricData holds a single metric waiting to be flushed.
type metricData struct {
	name       string
	value      float64
	unit       cwtypes.StandardUnit
	tags       []types.Tag
	timestamp  time.Time
	metricType metricType
}

// metricType indicates the type of metric.
type metricType int

const (
	metricTypeCounter metricType = iota
	metricTypeGauge
	metricTypeHistogram
)

// batcher accumulates metrics and converts them to CloudWatch format.
type batcher struct {
	namespace   string
	defaultTags []types.Tag
	buffer      []metricData
	maxSize     int
	mu          sync.Mutex
	aggregates  map[string]*aggregate // For histogram aggregation
}

// aggregate holds aggregated values for histograms.
type aggregate struct {
	count float64
	sum   float64
	min   float64
	max   float64
	tags  []types.Tag
}

// newBatcher creates a new batcher.
func newBatcher(namespace string, defaultTags []types.Tag, maxSize int) *batcher {
	return &batcher{
		namespace:   namespace,
		defaultTags: defaultTags,
		buffer:      make([]metricData, 0, maxSize),
		maxSize:     maxSize,
		aggregates:  make(map[string]*aggregate),
	}
}

// add adds a metric to the buffer.
func (b *batcher) add(md metricData) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if md.metricType == metricTypeHistogram {
		// Aggregate histograms
		key := b.aggregateKey(md.name, md.tags)
		if agg, exists := b.aggregates[key]; exists {
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
				tags:  md.tags,
			}
		}
		return len(b.buffer) >= b.maxSize
	}

	md.timestamp = time.Now()
	b.buffer = append(b.buffer, md)

	return len(b.buffer) >= b.maxSize
}

// aggregateKey creates a unique key for aggregating metrics.
func (b *batcher) aggregateKey(name string, tags []types.Tag) string {
	key := name
	for _, tag := range tags {
		key += "|" + tag.Key + "=" + tag.Value
	}
	return key
}

// drain removes and returns all buffered metrics as CloudWatch metric data.
func (b *batcher) drain() []cwtypes.MetricDatum {
	b.mu.Lock()
	defer b.mu.Unlock()

	var result []cwtypes.MetricDatum

	// Convert regular metrics
	for _, md := range b.buffer {
		dimensions := b.buildDimensions(md.tags)
		datum := cwtypes.MetricDatum{
			MetricName: &md.name,
			Value:      &md.value,
			Unit:       md.unit,
			Timestamp:  &md.timestamp,
			Dimensions: dimensions,
		}
		result = append(result, datum)
	}

	// Convert aggregated histograms
	now := time.Now()
	for name, agg := range b.aggregates {
		// Extract actual name from the aggregation key
		metricName := name
		for i, c := range name {
			if c == '|' {
				metricName = name[:i]
				break
			}
		}

		dimensions := b.buildDimensions(agg.tags)
		datum := cwtypes.MetricDatum{
			MetricName: &metricName,
			StatisticValues: &cwtypes.StatisticSet{
				SampleCount: &agg.count,
				Sum:         &agg.sum,
				Minimum:     &agg.min,
				Maximum:     &agg.max,
			},
			Unit:       cwtypes.StandardUnitMilliseconds,
			Timestamp:  &now,
			Dimensions: dimensions,
		}
		result = append(result, datum)
	}

	// Clear buffers
	b.buffer = make([]metricData, 0, b.maxSize)
	b.aggregates = make(map[string]*aggregate)

	return result
}

// buildDimensions converts tags to CloudWatch dimensions.
func (b *batcher) buildDimensions(tags []types.Tag) []cwtypes.Dimension {
	allTags := append(b.defaultTags, tags...)
	if len(allTags) == 0 {
		return nil
	}

	// CloudWatch has a limit of 30 dimensions
	if len(allTags) > 30 {
		allTags = allTags[:30]
	}

	dimensions := make([]cwtypes.Dimension, len(allTags))
	for i, tag := range allTags {
		name := tag.Key
		value := tag.Value
		dimensions[i] = cwtypes.Dimension{
			Name:  &name,
			Value: &value,
		}
	}
	return dimensions
}

// isEmpty returns true if there are no buffered metrics.
func (b *batcher) isEmpty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer) == 0 && len(b.aggregates) == 0
}
