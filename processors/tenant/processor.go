package tenant

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultTenantHeader is the app-visible header the tenant processor reads
// the tenant identifier from when Config.TenantHeader is unset. It is
// deliberately NOT the reserved x-bridge.tenant-id header: the runtime
// strips all reserved x-bridge.* headers at ingress (anti-spoofing), so a
// reserved source header would always be empty by the time the chain runs.
const DefaultTenantHeader = "x-tenant-id"

// Tenant observability metric names. Kept package-local — deliberately not
// added to the shared registry (domain/shared/metrics.go, which this module
// already imports) — because they are emitted only by this optional tenant
// processor. The shared registry holds the runtime's cross-cutting metric
// vocabulary; a single plugin's internal metric names do not belong there.
const (
	metricTenantTrackerErrors = "TenantTrackerErrors"
	metricTenantRejects       = "TenantRejects"
)

// ErrTenantHeaderReserved signals that the configured TenantHeader is a
// reserved x-bridge.* header. Such a header is stripped at ingress, so
// reading it would silently resolve no tenant. Rejected at construction
// as a permanent setup error so the misconfiguration fails fast.
var ErrTenantHeaderReserved = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "tenant: TenantHeader must not be a reserved x-bridge.* header",
}

// Processor validates tenant identity and enforces per-tenant quotas
// on messages flowing through the processing chain.
type Processor struct {
	config    Config
	validator ports.TenantValidator
	tracker   ports.TenantUsageTracker
	metrics   ports.MetricsExporter
	logger    *slog.Logger
}

var _ ports.Processor = (*Processor)(nil)

// New creates a tenant processor with the given configuration and options.
// The tenant source header defaults to DefaultTenantHeader (non-reserved)
// and a reserved x-bridge.* TenantHeader is rejected, so tenant resolution
// cannot silently fail against an ingress-stripped header.
func New(cfg Config, opts ...Option) (*Processor, error) {
	if cfg.TenantHeader == "" {
		cfg.TenantHeader = DefaultTenantHeader
	}
	if messaging.IsReservedHeader(cfg.TenantHeader) {
		return nil, ErrTenantHeaderReserved.With("header", cfg.TenantHeader)
	}
	if cfg.InFlightDecrementTimeout <= 0 {
		cfg.InFlightDecrementTimeout = 2 * time.Second
	}

	p := &Processor{config: cfg, metrics: &ports.NoopExporter{}}
	for _, opt := range opts {
		opt(p)
	}
	if p.metrics == nil {
		p.metrics = &ports.NoopExporter{}
	}
	return p, nil
}

func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "tenant"
}

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	tenantID, _ := messaging.GetHeaderString(env.Headers(), p.config.TenantHeader)

	if tenantID == "" {
		if p.config.RequireTenant {
			p.observeReject(ctx, "missing_required", tenantID)
			return shared.ErrInvalidPayload.WithMessage("tenant ID required")
		}
		return next(ctx, env)
	}

	if p.validator != nil {
		info, err := p.validator.Validate(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("tenant validation failed for %q: %w", tenantID, err)
		}

		if !info.Active {
			p.observeReject(ctx, "disabled", tenantID)
			return shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("tenant disabled: %s", tenantID),
			)
		}

		if info.MaxMessageSizeBytes > 0 && int64(len(env.Payload())) > info.MaxMessageSizeBytes {
			p.observeReject(ctx, "oversize", tenantID)
			return shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("message size %d exceeds tenant limit %d",
					len(env.Payload()), info.MaxMessageSizeBytes),
			)
		}
	}

	if p.tracker != nil {
		if err := p.tracker.IncrementInFlight(ctx, tenantID, 1); err != nil {
			p.observeTrackerError(ctx, "increment", tenantID, err)
			// Transient dependency failure: classify so the runtime
			// retries rather than DLQ-ing on a tracker hiccup.
			return shared.ErrUnavailable.Wrap(
				fmt.Errorf("tenant in-flight tracking failed: %w", err))
		}
		defer func() {
			decrementCtx := ctx
			if ctx.Err() != nil {
				var cancel context.CancelFunc
				decrementCtx, cancel = context.WithTimeout(context.Background(), p.config.InFlightDecrementTimeout)
				defer cancel()
			}
			if err := p.tracker.IncrementInFlight(decrementCtx, tenantID, -1); err != nil {
				// Best-effort cleanup: cannot alter control flow from a
				// defer, but the leaked in-flight count must be visible.
				p.observeTrackerError(decrementCtx, "decrement", tenantID, err)
			}
		}()
	}

	err := next(ctx, env)

	if p.tracker != nil && err == nil {
		if mErr := p.tracker.IncrementMessages(ctx, tenantID, 1); mErr != nil {
			// Message-count is advisory: swallowed for control flow
			// (Process still succeeds) but surfaced for observability.
			p.observeTrackerError(ctx, "message_count", tenantID, mErr)
		}
	}

	return err
}

// observeTrackerError emits a metric and a structured warning when a
// per-tenant usage-tracker call fails. op is a low-cardinality dimension
// (increment / decrement / message_count); the tenant ID is kept out of
// the metric tags (unbounded cardinality) and logged instead.
func (p *Processor) observeTrackerError(ctx context.Context, op, tenantID string, err error) {
	p.metrics.Counter(metricTenantTrackerErrors, 1, shared.Tag{Key: "op", Value: op})
	if p.logger != nil {
		p.logger.WarnContext(ctx, "tenant usage tracker error",
			"processor", p.Name(), "op", op, "tenant", tenantID, "error", err)
	}
}

// observeReject emits a metric and a structured warning when the
// processor rejects or skips a message for a tenancy-policy reason. The
// reason is low-cardinality; the tenant ID is logged, not tagged.
func (p *Processor) observeReject(ctx context.Context, reason, tenantID string) {
	p.metrics.Counter(metricTenantRejects, 1, shared.Tag{Key: "reason", Value: reason})
	if p.logger != nil {
		p.logger.WarnContext(ctx, "tenant message rejected",
			"processor", p.Name(), "reason", reason, "tenant", tenantID)
	}
}
