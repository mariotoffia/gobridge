package cloudwatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
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
		client, err := newCloudWatchClient(ctx, e.config)
		if err != nil {
			return nil, fmt.Errorf("cloudwatch: %w", err)
		}
		e.client = client
	}

	e.batcher = newBatcher(e.config.Namespace, e.config.DefaultTags, e.config.BufferSize, e.config.Clock, e.config.Logger, e.config.MaxRetryDatums)

	e.wg.Add(1)
	go e.flushLoop()

	return e, nil
}

func (e *Exporter) Counter(name string, value int64, tags ...shared.Tag) {
	if e.batcher.addCounter(name, value, tags) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Gauge(name string, value float64, tags ...shared.Tag) {
	if e.batcher.addGauge(name, value, tags) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Histogram(name string, value float64, tags ...shared.Tag) {
	if e.batcher.addHistogram(name, value, tags) {
		go e.asyncFlush()
	}
}

func (e *Exporter) Timer(name string, duration time.Duration, tags ...shared.Tag) {
	if e.batcher.addTimer(name, duration, tags) {
		go e.asyncFlush()
	}
}

// Flush sends all buffered metrics to CloudWatch immediately.
func (e *Exporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.batcher.flush(ctx, e.client, e.config.Namespace, e.config.MaxBatchSize)
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

func (e *Exporter) flushLoop() {
	defer e.wg.Done()
	ticker := e.config.Clock.NewTicker(e.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C():
			ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushRPCTimeout)
			if err := e.Flush(ctx); err != nil && e.config.Logger != nil {
				e.config.Logger.Warn("cloudwatch: periodic flush failed; samples requeued for retry",
					slog.String("error", err.Error()))
			}
			cancel()
		}
	}
}

func (e *Exporter) asyncFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushRPCTimeout)
	defer cancel()
	if err := e.Flush(ctx); err != nil && e.config.Logger != nil {
		e.config.Logger.Warn("cloudwatch: async flush failed; samples requeued for retry",
			slog.String("error", err.Error()))
	}
}
