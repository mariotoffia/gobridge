package cloudwatch

import (
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Config holds the configuration for the CloudWatch metrics exporter.
type Config struct {
	Region          string        `json:"region,omitempty"`
	Namespace       string        `json:"namespace"`
	DefaultTags     []shared.Tag  `json:"defaultTags,omitempty"`
	FlushInterval   time.Duration `json:"flushInterval,omitempty"`
	FlushRPCTimeout time.Duration `json:"flushRPCTimeout,omitempty"`
	BufferSize      int           `json:"bufferSize,omitempty"`
	MaxBatchSize    int           `json:"maxBatchSize,omitempty"`
	// MaxRetryDatums bounds how many metric datums a failed PutMetricData is
	// allowed to requeue for the next flush. Beyond this the oldest requeued
	// datums are dropped (and counted) so a persistently failing CloudWatch
	// endpoint cannot grow memory without bound. Default: 10000.
	MaxRetryDatums int          `json:"maxRetryDatums,omitempty"`
	Endpoint       string       `json:"endpoint,omitempty"`
	Clock          clock.Clock  `json:"-"`
	Logger         *slog.Logger `json:"-"`
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
func WithDefaultTags(tags ...shared.Tag) Option {
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

// WithLogger sets the structured logger used to warn about dropped/requeued
// metrics and invalid dimensions. Nil is safe: diagnostics are suppressed.
func WithLogger(l *slog.Logger) Option {
	return func(e *Exporter) { e.config.Logger = l }
}

// WithMaxRetryDatums bounds how many datums a failed PutMetricData may
// requeue for the next flush before the oldest are dropped. Default: 10000.
func WithMaxRetryDatums(n int) Option {
	return func(e *Exporter) { e.config.MaxRetryDatums = n }
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
	if cfg.MaxRetryDatums <= 0 {
		// Zero is the unset default; a negative value must not be interpreted
		// as "disable the bound" (the requeue guard is maxRetry > 0), which
		// would let the retry buffer grow without limit (N5).
		cfg.MaxRetryDatums = 10000
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
}
