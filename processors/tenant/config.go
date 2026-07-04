package tenant

import (
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Config holds the tenant processor configuration.
//
// The yaml/json tags define the serialized key names so a future
// configuration surface (file, blueprint, HTTP API) can decode directly
// into this struct — same convention as circuitbreaker.Config. No YAML
// decoding pipeline exists today; processors are constructed in Go and
// referenced by name from route definitions. The validator, tracker,
// metrics, and logger dependencies are Go-only options and are never
// serialized.
type Config struct {
	Name                     string        `json:"name" yaml:"name"`
	TenantHeader             string        `json:"tenantHeader,omitempty" yaml:"tenantHeader,omitempty"`
	RequireTenant            bool          `json:"requireTenant,omitempty" yaml:"requireTenant,omitempty"`
	InFlightDecrementTimeout time.Duration `json:"inFlightDecrementTimeout,omitempty" yaml:"inFlightDecrementTimeout,omitempty"`
}

// Option configures the tenant processor.
type Option func(*Processor)

// WithValidator sets the tenant validator used for tenant lookup, the
// active check, and the MaxMessageSizeBytes limit check. When nil, the
// processor skips validation.
func WithValidator(v ports.TenantValidator) Option {
	return func(p *Processor) { p.validator = v }
}

// WithUsageTracker enables per-tenant usage tracking. When nil, the
// processor skips in-flight and message count tracking.
func WithUsageTracker(t ports.TenantUsageTracker) Option {
	return func(p *Processor) { p.tracker = t }
}

// WithMetrics sets the metrics exporter used to emit tracker-error and
// tenant-reject counters. When unset, a NoopExporter is used.
func WithMetrics(m ports.MetricsExporter) Option {
	return func(p *Processor) { p.metrics = m }
}

// WithLogger sets the structured logger used to surface tracker failures
// and tenant rejects. When nil (default), those log hooks are skipped.
func WithLogger(l *slog.Logger) Option {
	return func(p *Processor) { p.logger = l }
}
