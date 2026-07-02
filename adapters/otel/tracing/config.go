package oteltracing

import (
	"os"
	"time"
)

// Config holds the configuration for the OTel tracing adapter.
//
// Environment variables (K7): when Endpoint / ServiceName are left
// unset via options, the adapter honors the standard OpenTelemetry
// environment variables before falling back to built-in defaults.
// Precedence is: explicit WithXxx option > OTEL_* env var > default.
//   - OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
//   - OTEL_EXPORTER_OTLP_HEADERS (honored by the SDK exporter)
//   - OTEL_SERVICE_NAME
//   - OTEL_RESOURCE_ATTRIBUTES (honored via resource.Default())
type Config struct {
	Endpoint       string `json:"endpoint,omitempty"`
	ServiceName    string `json:"serviceName,omitempty"`
	ServiceVersion string `json:"serviceVersion,omitempty"`
	Environment    string `json:"environment,omitempty"`
	// SamplerRatio is the head sampling ratio in [0,1]. A nil pointer
	// means "unset" and defaults to 1.0; an explicit 0.0 disables
	// sampling of new (root) traces (K6).
	SamplerRatio *float64          `json:"samplerRatio,omitempty"`
	Insecure     bool              `json:"insecure,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`

	// Batch/queue/export tuning for the BatchSpanProcessor (K3). Zero
	// values keep the OTel SDK defaults.
	BatchTimeout       time.Duration `json:"batchTimeout,omitempty"`
	ExportTimeout      time.Duration `json:"exportTimeout,omitempty"`
	MaxQueueSize       int           `json:"maxQueueSize,omitempty"`
	MaxExportBatchSize int           `json:"maxExportBatchSize,omitempty"`

	// errorHandler receives export/backpressure failures that would
	// otherwise be invisible (K3). Not serialized.
	errorHandler func(error)
}

// Option is a functional option for configuring the tracer.
type Option func(*Tracer)

// WithEndpoint sets the OTLP collector endpoint.
func WithEndpoint(endpoint string) Option {
	return func(t *Tracer) {
		t.config.Endpoint = endpoint
	}
}

// WithServiceName sets the service name resource attribute.
func WithServiceName(name string) Option {
	return func(t *Tracer) {
		t.config.ServiceName = name
	}
}

// WithServiceVersion sets the service version resource attribute.
func WithServiceVersion(version string) Option {
	return func(t *Tracer) {
		t.config.ServiceVersion = version
	}
}

// WithEnvironment sets the deployment environment resource attribute.
func WithEnvironment(env string) Option {
	return func(t *Tracer) {
		t.config.Environment = env
	}
}

// WithSamplerRatio sets the trace sampling ratio (0.0 to 1.0). A ratio
// of 0.0 disables sampling of new traces; New validates the range.
func WithSamplerRatio(ratio float64) Option {
	return func(t *Tracer) {
		r := ratio
		t.config.SamplerRatio = &r
	}
}

// WithInsecure enables HTTP instead of HTTPS for the exporter.
func WithInsecure() Option {
	return func(t *Tracer) {
		t.config.Insecure = true
	}
}

// WithHeaders sets additional HTTP headers on the exporter.
func WithHeaders(headers map[string]string) Option {
	return func(t *Tracer) {
		t.config.Headers = headers
	}
}

// WithBatchTimeout sets the maximum delay before a non-full batch of
// spans is exported.
func WithBatchTimeout(d time.Duration) Option {
	return func(t *Tracer) {
		t.config.BatchTimeout = d
	}
}

// WithExportTimeout sets the maximum duration a single span export may
// take before it is cancelled.
func WithExportTimeout(d time.Duration) Option {
	return func(t *Tracer) {
		t.config.ExportTimeout = d
	}
}

// WithMaxQueueSize bounds the number of spans buffered before the
// processor starts dropping (backpressure).
func WithMaxQueueSize(n int) Option {
	return func(t *Tracer) {
		t.config.MaxQueueSize = n
	}
}

// WithMaxExportBatchSize bounds the number of spans exported per batch.
func WithMaxExportBatchSize(n int) Option {
	return func(t *Tracer) {
		t.config.MaxExportBatchSize = n
	}
}

// WithErrorHandler installs a callback invoked when a span export
// fails. Without it, export/backpressure failures are invisible (K3).
func WithErrorHandler(fn func(error)) Option {
	return func(t *Tracer) {
		t.config.errorHandler = fn
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Endpoint == "" && envEndpointUnset() {
		cfg.Endpoint = "http://localhost:4318"
	}
	if cfg.ServiceName == "" && os.Getenv("OTEL_SERVICE_NAME") == "" {
		cfg.ServiceName = "gobridge"
	}
	if cfg.SamplerRatio == nil {
		one := 1.0
		cfg.SamplerRatio = &one
	}
}

// envEndpointUnset reports whether neither the generic nor the
// traces-specific OTLP endpoint environment variable is set.
func envEndpointUnset() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == ""
}
