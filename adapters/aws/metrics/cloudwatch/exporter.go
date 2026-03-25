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
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Exporter implements [ports.MetricsExporter] for AWS CloudWatch.
type Exporter struct {
	config    Config
	client    cloudWatchAPI
	batcher   *batcher
	stopCh    chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	closeOnce sync.Once
}

var _ ports.MetricsExporter = (*Exporter)(nil)

// New creates a new CloudWatch metrics exporter. The namespace is used
// as the CloudWatch metric namespace (e.g. "GoBridge/Runtime").
func New(ctx context.Context, namespace string, opts ...Option) (*Exporter, error) {
	e := &Exporter{
		config: Config{Namespace: namespace},
		stopCh: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(e)
	}
	applyDefaults(&e.config)

	if e.client == nil {
		cfg, err := loadAWSConfig(ctx, e.config)
		if err != nil {
			return nil, fmt.Errorf("cloudwatch: load AWS config: %w", err)
		}
		e.client = cloudwatch.NewFromConfig(cfg)
	}

	e.batcher = newBatcher(e.config.Namespace, e.config.DefaultTags, e.config.BufferSize)

	e.wg.Add(1)
	go e.flushLoop()

	return e, nil
}

func (e *Exporter) Counter(name string, value int64, tags ...domain.Tag) {
	if e.batcher.add(metricData{
		name:       name,
		value:      float64(value),
		unit:       cwtypes.StandardUnitCount,
		tags:       tags,
		metricType: metricTypeCounter,
	}) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Gauge(name string, value float64, tags ...domain.Tag) {
	if e.batcher.add(metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeGauge,
	}) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Histogram(name string, value float64, tags ...domain.Tag) {
	if e.batcher.add(metricData{
		name:       name,
		value:      value,
		unit:       cwtypes.StandardUnitNone,
		tags:       tags,
		metricType: metricTypeHistogram,
	}) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Timer(name string, duration time.Duration, tags ...domain.Tag) {
	ms := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	if e.batcher.add(metricData{
		name:       name,
		value:      ms,
		unit:       cwtypes.StandardUnitMilliseconds,
		tags:       tags,
		metricType: metricTypeHistogram,
	}) {
		go e.asyncFlush()
	}
}

// Flush sends all buffered metrics to CloudWatch immediately.
func (e *Exporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.batcher.isEmpty() {
		return nil
	}
	return e.sendBatched(ctx, e.batcher.drain())
}

// Close stops the background flush loop and performs a final flush.
// It is safe to call multiple times.
func (e *Exporter) Close(ctx context.Context) error {
	var err error
	e.closeOnce.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
		err = e.Flush(ctx)
	})
	return err
}

func (e *Exporter) sendBatched(ctx context.Context, data []cwtypes.MetricDatum) error {
	batch := e.config.MaxBatchSize
	for i := 0; i < len(data); i += batch {
		end := i + batch
		if end > len(data) {
			end = len(data)
		}
		_, err := e.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(e.config.Namespace),
			MetricData: data[i:end],
		})
		if err != nil {
			return fmt.Errorf("cloudwatch: PutMetricData: %w", err)
		}
	}
	return nil
}

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

func (e *Exporter) asyncFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.Flush(ctx)
}

func loadAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}
	if cfg.Endpoint != "" {
		awsCfg.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return awsCfg, nil
}
