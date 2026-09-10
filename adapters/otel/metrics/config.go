package otelmetrics

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TagKeyInstanceID is the attribute key used by WithInstanceTag to
// distinguish bridge instances in a fleet (mirrors
// ports.BridgeSettings.InstanceID / UBIQUITOUS.md "BridgeSettings").
// Adapter-local constant, same precedent as
// adapters/aws/transport/sqs.TagKeyQueueURL.
const TagKeyInstanceID = "instance_id"

// defaultMaxInstruments bounds the per-name instrument cache to guard
// against unbounded cardinality from dynamic metric names.
// ponytail: 1024 distinct instrument names is far above the bounded,
// static set the runtime emits (see domain/shared/metrics.go); dynamic
// names beyond this ceiling are rejected and surfaced via the error
// handler rather than silently growing the process heap.
const defaultMaxInstruments = 1024

// Config holds the configuration for the OTEL metrics exporter.
//
// Environment variables: when Endpoint / ServiceName are left
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
	// InstanceID, when set, is added to DefaultTags as the
	// "instance_id" attribute so per-instance series in a fleet do not
	// collide. Set via WithInstanceTag.
	InstanceID string `json:"instanceId,omitempty"`
	// FlushInterval is how often to export metrics. Default: 60 seconds.
	FlushInterval time.Duration `json:"flushInterval,omitempty"`
	// ExportTimeout bounds a single periodic export. Zero keeps the SDK
	// default.
	ExportTimeout time.Duration `json:"exportTimeout,omitempty"`
	// MaxInstruments bounds the instrument cache to reject unbounded
	// dynamic metric names. Zero means defaultMaxInstruments.
	MaxInstruments int `json:"maxInstruments,omitempty"`
	// Insecure uses HTTP instead of HTTPS. Default: false.
	Insecure bool `json:"insecure,omitempty"`
	// Headers are additional headers for the exporter.
	Headers map[string]string `json:"headers,omitempty"`

	// errorHandler receives export/backpressure failures and rejected
	// instrument creations that would otherwise be invisible.
	// Defaults to a slog.Default() Warn logger; explicitly
	// installing nil suppresses reporting. Not serialized.
	errorHandler func(error)
	// errorHandlerSet distinguishes "never configured" (default warn
	// logging applies) from an explicit WithErrorHandler(nil) opt-out.
	errorHandlerSet bool
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

// WithInstanceTag adds an "instance_id" attribute (TagKeyInstanceID) to
// every emitted metric so per-instance series in a fleet do not collide
// Pass the bridge's configured InstanceID
// (ports.BridgeSettings.InstanceID); an empty id derives
// "<hostname>-<pid>".
func WithInstanceTag(id string) Option {
	return func(e *Exporter) {
		if id == "" {
			id = deriveInstanceID()
		}
		e.config.InstanceID = id
	}
}

// deriveInstanceID builds a best-effort instance identity from
// hostname and pid, for callers that do not configure an explicit
// ports.BridgeSettings.InstanceID.
func deriveInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
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
// the bound are rejected and surfaced via the error handler.
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
// fails or an instrument is rejected. When never configured, failures
// are logged at Warn level via slog.Default(); pass nil to
// explicitly suppress reporting.
func WithErrorHandler(fn func(error)) Option {
	return func(e *Exporter) {
		e.config.errorHandler = fn
		e.config.errorHandlerSet = true
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
	if cfg.InstanceID != "" && !hasTagKey(cfg.DefaultTags, TagKeyInstanceID) {
		cfg.DefaultTags = append(cfg.DefaultTags, shared.Tag{Key: TagKeyInstanceID, Value: cfg.InstanceID})
	}
	if cfg.errorHandler == nil && !cfg.errorHandlerSet {
		// Export failures and rejected instruments must not be silent
		// by default. Opt out with WithErrorHandler(nil).
		cfg.errorHandler = func(err error) {
			slog.Default().Warn("otel-metrics: export failure", slog.String("error", err.Error()))
		}
	}
}

func hasTagKey(tags []shared.Tag, key string) bool {
	for _, t := range tags {
		if t.Key == key {
			return true
		}
	}
	return false
}

// envEndpointUnset reports whether neither the generic nor the
// metrics-specific OTLP endpoint environment variable is set.
func envEndpointUnset() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == ""
}
