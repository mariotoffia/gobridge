package tenant

import (
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Config holds the tenant processor configuration.
type Config struct {
	Name                    string
	TenantHeader            string
	RequireTenant           bool
	InFlightDecrementTimeout time.Duration
}

// Option configures the tenant processor.
type Option func(*Processor)

// WithValidator sets the tenant validator used for tenant lookup and
// active/quota checks. When nil, the processor skips validation.
func WithValidator(v ports.TenantValidator) Option {
	return func(p *Processor) { p.validator = v }
}

// WithUsageTracker enables per-tenant usage tracking. When nil, the
// processor skips in-flight and message count tracking.
func WithUsageTracker(t ports.TenantUsageTracker) Option {
	return func(p *Processor) { p.tracker = t }
}
