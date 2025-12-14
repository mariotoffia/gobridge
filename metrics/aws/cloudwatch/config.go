package cloudwatch

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// Config holds the configuration for the CloudWatch metrics exporter.
type Config struct {
	// Region is the AWS region to use. If empty, uses default from environment.
	Region string `json:"region,omitempty"`
	// Namespace is the CloudWatch metric namespace (required).
	// Example: "MyApp/Bridge"
	Namespace string `json:"namespace"`
	// DefaultTags are added to all metrics as dimensions.
	DefaultTags []types.Tag `json:"defaultTags,omitempty"`
	// FlushInterval is how often to flush buffered metrics. Default: 60 seconds.
	FlushInterval time.Duration `json:"flushInterval,omitempty"`
	// BufferSize is the maximum number of metrics to buffer. Default: 1000.
	BufferSize int `json:"bufferSize,omitempty"`
	// MaxBatchSize is the max metrics per CloudWatch API call. Default: 20 (CW limit).
	MaxBatchSize int `json:"maxBatchSize,omitempty"`
	// Client is an optional pre-configured CloudWatch client.
	Client *cloudwatch.Client `json:"-"`
	// Endpoint is an optional custom endpoint (for LocalStack, etc).
	Endpoint string `json:"endpoint,omitempty"`
}

// Option is a functional option for configuring the exporter.
type Option func(*Exporter)

// WithRegion sets the AWS region.
func WithRegion(region string) Option {
	return func(e *Exporter) {
		e.config.Region = region
	}
}

// WithNamespace sets the CloudWatch metric namespace.
func WithNamespace(namespace string) Option {
	return func(e *Exporter) {
		e.config.Namespace = namespace
	}
}

// WithDefaultTags sets the default tags for all metrics.
func WithDefaultTags(tags ...types.Tag) Option {
	return func(e *Exporter) {
		e.config.DefaultTags = tags
	}
}

// WithFlushInterval sets the flush interval.
func WithFlushInterval(interval time.Duration) Option {
	return func(e *Exporter) {
		e.config.FlushInterval = interval
	}
}

// WithBufferSize sets the buffer size.
func WithBufferSize(size int) Option {
	return func(e *Exporter) {
		e.config.BufferSize = size
	}
}

// WithClient sets a pre-configured CloudWatch client.
func WithClient(client *cloudwatch.Client) Option {
	return func(e *Exporter) {
		e.config.Client = client
		e.client = client
	}
}

// WithEndpoint sets a custom endpoint (for LocalStack, etc).
func WithEndpoint(endpoint string) Option {
	return func(e *Exporter) {
		e.config.Endpoint = endpoint
	}
}

// applyDefaults fills in default values for config.
func applyDefaults(cfg *Config) {
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 60 * time.Second
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = 1000
	}
	if cfg.MaxBatchSize == 0 {
		cfg.MaxBatchSize = 20 // CloudWatch limit
	}
}
