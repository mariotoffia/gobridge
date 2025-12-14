package cloudwatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// Exporter implements types.MetricsExporter for AWS CloudWatch.
type Exporter struct {
	config  Config
	client  *cloudwatch.Client
	batcher *batcher
	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
}

// New creates a new CloudWatch metrics exporter.
func New(ctx context.Context, namespace string, opts ...Option) (*Exporter, error) {
	e := &Exporter{
		config: Config{
			Namespace: namespace,
		},
		stopCh: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(e)
	}

	applyDefaults(&e.config)

	// Create CloudWatch client if not provided
	if e.client == nil {
		cfg, err := e.loadAWSConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		e.client = cloudwatch.NewFromConfig(cfg)
	}

	e.batcher = newBatcher(e.config.Namespace, e.config.DefaultTags, e.config.BufferSize)

	// Start background flushing
	e.wg.Add(1)
	go e.flushLoop()

	return e, nil
}

// Counter increments a counter metric.
func (e *Exporter) Counter(name string, value int64, tags ...types.Tag) {
	md := metricData{
		name:       name,
		value:      float64(value),
		unit:       cwtypes.StandardUnitCount,
		tags:       tags,
		metricType: metricTypeCounter,
	}

	if e.batcher.add(md) {
		// Buffer full, trigger async flush
		go e.asyncFlush()
	}
}

// Gauge sets a gauge metric value.
func (e *Exporter) Gauge(name string, value float64, tags ...types.Tag) {
	md := metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeGauge,
	}

	if e.batcher.add(md) {
		go e.asyncFlush()
	}
}

// Histogram records a histogram value.
func (e *Exporter) Histogram(name string, value float64, tags ...types.Tag) {
	md := metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeHistogram,
	}

	if e.batcher.add(md) {
		go e.asyncFlush()
	}
}

// Timer records a duration.
func (e *Exporter) Timer(name string, duration time.Duration, tags ...types.Tag) {
	// Convert to milliseconds
	ms := float64(duration.Nanoseconds()) / float64(time.Millisecond)

	md := metricData{
		name:       name,
		value:      ms,
		unit:       cwtypes.StandardUnitMilliseconds,
		tags:       tags,
		metricType: metricTypeHistogram,
	}

	if e.batcher.add(md) {
		go e.asyncFlush()
	}
}

// Flush sends all buffered metrics to CloudWatch.
func (e *Exporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.batcher.isEmpty() {
		return nil
	}

	data := e.batcher.drain()
	return e.sendBatched(ctx, data)
}

// sendBatched sends metrics in batches respecting CloudWatch limits.
func (e *Exporter) sendBatched(ctx context.Context, data []cwtypes.MetricDatum) error {
	maxBatch := e.config.MaxBatchSize

	for i := 0; i < len(data); i += maxBatch {
		end := i + maxBatch
		if end > len(data) {
			end = len(data)
		}

		batch := data[i:end]
		_, err := e.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(e.config.Namespace),
			MetricData: batch,
		})
		if err != nil {
			return fmt.Errorf("failed to put metric data: %w", err)
		}
	}

	return nil
}

// Close stops the exporter and flushes remaining metrics.
func (e *Exporter) Close(ctx context.Context) error {
	close(e.stopCh)
	e.wg.Wait()

	// Final flush
	return e.Flush(ctx)
}

// flushLoop periodically flushes metrics.
func (e *Exporter) flushLoop() {
	defer e.wg.Done()

	ticker := time.NewTicker(e.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = e.Flush(ctx)
			cancel()
		}
	}
}

// asyncFlush performs an async flush when buffer is full.
func (e *Exporter) asyncFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.Flush(ctx)
}

// loadAWSConfig loads AWS configuration.
func (e *Exporter) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}

	if e.config.Region != "" {
		opts = append(opts, awsconfig.WithRegion(e.config.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}

	// Apply custom endpoint if provided
	if e.config.Endpoint != "" {
		cfg.BaseEndpoint = aws.String(e.config.Endpoint)
	}

	return cfg, nil
}

// Ensure Exporter implements types.MetricsExporter
var _ types.MetricsExporter = (*Exporter)(nil)
