package otelmetrics

import (
	"os"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// defaultMaxInstruments bounds the per-name instrument cache to guard
// against unbounded cardinality from dynamic metric names (K9).
// ponytail: 1024 distinct instrument names is far above the bounded,
// static set the runtime emits (see domain/shared/metrics.go); dynamic
// names beyond this ceiling are rejected and surfaced via the error
// handler rather than silently growing the process heap.
const defaultMaxInstruments = 1024

// Config holds the configuration for the OTEL metrics exporter.
//
// Environment variables (K7): when Endpoint / ServiceName are left
// unset via options, the adapter honors the standard OpenTelemetry
// environment variables before falling back to built-in defaults.
// Precedence is: explicit WithXxx option > OTEL_* env var > default.
//   - OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
//   - OTEL_EXPORTER_OTLP_HEADERS (honored by the SDK exporter)
//   - OTEL_SERVICE_NAME
//   - OTEL_RESOURCE_ATTRIBUTES (honored via resource.Default())
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
	DefaultTags []shared.Tag `json:"defaultTags,omitempty"`
	// FlushInterval is how often to export metrics. Default: 60 seconds.
	FlushInterval time.Duration `json:"flushInterval,omitempty"`
	// ExportTimeout bounds a single periodic export. Zero keeps the SDK
	// default (K3).
	ExportTimeout time.Duration `json:"exportTimeout,omitempty"`
	// MaxInstruments bounds the instrument cache to reject unbounded
	// dynamic metric names (K9). Zero means defaultMaxInstruments.
	MaxInstruments int `json:"maxInstruments,omitempty"`
	// Insecure uses HTTP instead of HTTPS. Default: false.
	Insecure bool `json:"insecure,omitempty"`
	// Headers are additional headers for the exporter.
	Headers map[string]string `json:"headers,omitempty"`

	// errorHandler receives export/backpressure failures and rejected
	// instrument creations that would otherwise be invisible (K3, K9).
	// Not serialized.
	errorHandler func(error)
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
func WithDefaultTags(tags ...shared.Tag) Option {
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

// WithExportTimeout bounds the duration of a single periodic export.
func WithExportTimeout(d time.Duration) Option {
	return func(e *Exporter) {
		e.config.ExportTimeout = d
	}
}

// WithMaxInstruments bounds the number of distinct metric instruments
// (counters+gauges+histograms) the exporter will create. Emits beyond
// the bound are rejected and surfaced via the error handler (K9).
func WithMaxInstruments(n int) Option {
	return func(e *Exporter) {
		e.config.MaxInstruments = n
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

// WithErrorHandler installs a callback invoked when a metric export
// fails or an instrument is rejected. Without it these failures are
// invisible (K3, K9).
func WithErrorHandler(fn func(error)) Option {
	return func(e *Exporter) {
		e.config.errorHandler = fn
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Endpoint == "" && envEndpointUnset() {
		cfg.Endpoint = "http://localhost:4318"
	}
	if cfg.ServiceName == "" && os.Getenv("OTEL_SERVICE_NAME") == "" {
		cfg.ServiceName = "gobridge"
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 60 * time.Second
	}
	if cfg.MaxInstruments == 0 {
		cfg.MaxInstruments = defaultMaxInstruments
	}
}

// envEndpointUnset reports whether neither the generic nor the
// metrics-specific OTLP endpoint environment variable is set.
func envEndpointUnset() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == ""
}
