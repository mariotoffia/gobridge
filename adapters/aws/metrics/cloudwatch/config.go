package cloudwatch

import (
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
)

// Config holds the configuration for the CloudWatch metrics exporter.
type Config struct {
	Region         string        `json:"region,omitempty"`
	Namespace      string        `json:"namespace"`
	DefaultTags    []domain.Tag  `json:"defaultTags,omitempty"`
	FlushInterval  time.Duration `json:"flushInterval,omitempty"`
	FlushRPCTimeout time.Duration `json:"flushRPCTimeout,omitempty"`
	BufferSize     int           `json:"bufferSize,omitempty"`
	MaxBatchSize   int           `json:"maxBatchSize,omitempty"`
	Endpoint       string        `json:"endpoint,omitempty"`
	Clock          clock.Clock   `json:"-"`
}

// Option is a functional option for configuring the exporter.
type Option func(*Exporter)

// WithRegion sets the AWS region.
func WithRegion(region string) Option {
	return func(e *Exporter) { e.config.Region = region }
}

// WithNamespace overrides the CloudWatch metric namespace.
func WithNamespace(namespace string) Option {
	return func(e *Exporter) { e.config.Namespace = namespace }
}

// WithDefaultTags sets the default tags added to all metrics as dimensions.
func WithDefaultTags(tags ...domain.Tag) Option {
	return func(e *Exporter) { e.config.DefaultTags = tags }
}

// WithFlushInterval sets how often buffered metrics are flushed. Default: 60s.
func WithFlushInterval(interval time.Duration) Option {
	return func(e *Exporter) { e.config.FlushInterval = interval }
}

// WithBufferSize sets the maximum number of non-histogram metrics to buffer
// before triggering an async flush. Default: 1000.
func WithBufferSize(size int) Option {
	return func(e *Exporter) { e.config.BufferSize = size }
}

// WithClient sets a pre-configured CloudWatch client, bypassing automatic
// credential resolution. Useful for testing with LocalStack or mocks.
func WithClient(client cloudWatchAPI) Option {
	return func(e *Exporter) {
		e.client = client
	}
}

// WithFlushRPCTimeout sets the per-RPC timeout used when flushing
// metrics to CloudWatch. Defaults to FlushInterval / 2.
func WithFlushRPCTimeout(d time.Duration) Option {
	return func(e *Exporter) { e.config.FlushRPCTimeout = d }
}

// WithClock overrides the clock used for flush tickers and timeouts.
// Primarily intended for tests; production code should rely on the
// default clock.System.
func WithClock(c clock.Clock) Option {
	return func(e *Exporter) {
		if c != nil {
			e.config.Clock = c
		}
	}
}

// WithEndpoint sets a custom endpoint URL (e.g. for LocalStack).
func WithEndpoint(endpoint string) Option {
	return func(e *Exporter) { e.config.Endpoint = endpoint }
}

func applyDefaults(cfg *Config) {
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 60 * time.Second
	}
	if cfg.FlushRPCTimeout <= 0 {
		cfg.FlushRPCTimeout = min(cfg.FlushInterval/2, 30*time.Second)
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = 1000
	}
	if cfg.MaxBatchSize == 0 {
		cfg.MaxBatchSize = 20 // CloudWatch PutMetricData limit
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
}
