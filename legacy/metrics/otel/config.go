package otel

import (
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Config holds the configuration for the OTEL metrics exporter.
type Config struct {
	// Endpoint is the OTLP collector endpoint.
	// Example: "http://localhost:4318" (HTTP) or "localhost:4317" (gRPC)
	Endpoint string `json:"endpoint,omitempty"`
	// ServiceName is the service.name resource attribute.
	ServiceName string `json:"serviceName,omitempty"`
	// ServiceVersion is the service.version resource attribute.
	ServiceVersion string `json:"serviceVersion,omitempty"`
	// Environment is the deployment.environment resource attribute.
	Environment string `json:"environment,omitempty"`
	// DefaultTags are added to all metrics as attributes.
	DefaultTags []types.Tag `json:"defaultTags,omitempty"`
	// FlushInterval is how often to export metrics. Default: 60 seconds.
	FlushInterval time.Duration `json:"flushInterval,omitempty"`
	// Insecure uses HTTP instead of HTTPS. Default: false.
	Insecure bool `json:"insecure,omitempty"`
	// Headers are additional headers for the exporter.
	Headers map[string]string `json:"headers,omitempty"`
}

// Option is a functional option for configuring the exporter.
type Option func(*Exporter)

// WithEndpoint sets the OTLP collector endpoint.
func WithEndpoint(endpoint string) Option {
	return func(e *Exporter) {
		e.config.Endpoint = endpoint
	}
}

// WithServiceName sets the service name.
func WithServiceName(name string) Option {
	return func(e *Exporter) {
		e.config.ServiceName = name
	}
}

// WithServiceVersion sets the service version.
func WithServiceVersion(version string) Option {
	return func(e *Exporter) {
		e.config.ServiceVersion = version
	}
}

// WithEnvironment sets the environment.
func WithEnvironment(env string) Option {
	return func(e *Exporter) {
		e.config.Environment = env
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

// WithInsecure enables HTTP instead of HTTPS.
func WithInsecure() Option {
	return func(e *Exporter) {
		e.config.Insecure = true
	}
}

// WithHeaders sets additional headers.
func WithHeaders(headers map[string]string) Option {
	return func(e *Exporter) {
		e.config.Headers = headers
	}
}

// applyDefaults fills in default values for config.
func applyDefaults(cfg *Config) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:4318"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "gobridge"
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 60 * time.Second
	}
}
