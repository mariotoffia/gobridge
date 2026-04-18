package tenant

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Processor validates tenant identity and enforces per-tenant quotas
// on messages flowing through the processing chain.
type Processor struct {
	config    Config
	validator ports.TenantValidator
	tracker   ports.TenantUsageTracker
}

var _ ports.Processor = (*Processor)(nil)

// New creates a tenant processor with the given configuration and options.
func New(cfg Config, opts ...Option) *Processor {
	if cfg.TenantHeader == "" {
		cfg.TenantHeader = domain.HeaderTenantID
	}
	if cfg.InFlightDecrementTimeout <= 0 {
		cfg.InFlightDecrementTimeout = 2 * time.Second
	}

	p := &Processor{config: cfg}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "tenant"
}

func (p *Processor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	tenantID, _ := domain.GetHeaderString(env.Headers, p.config.TenantHeader)

	if tenantID == "" {
		if p.config.RequireTenant {
			return domain.ErrInvalidPayload.WithMessage("tenant ID required")
		}
		return next(ctx, env)
	}

	if p.validator != nil {
		info, err := p.validator.Validate(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("tenant validation failed for %q: %w", tenantID, err)
		}

		if !info.Active {
			return domain.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("tenant disabled: %s", tenantID),
			)
		}

		if info.MaxMessageSizeBytes > 0 && int64(len(env.Payload)) > info.MaxMessageSizeBytes {
			return domain.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("message size %d exceeds tenant limit %d",
					len(env.Payload), info.MaxMessageSizeBytes),
			)
		}
	}

	if p.tracker != nil {
		if err := p.tracker.IncrementInFlight(ctx, tenantID, 1); err != nil {
			return fmt.Errorf("tenant in-flight tracking failed: %w", err)
		}
		defer func() {
			decrementCtx := ctx
			if ctx.Err() != nil {
				var cancel context.CancelFunc
				decrementCtx, cancel = context.WithTimeout(context.Background(), p.config.InFlightDecrementTimeout)
				defer cancel()
			}
			_ = p.tracker.IncrementInFlight(decrementCtx, tenantID, -1)
		}()
	}

	err := next(ctx, env)

	if p.tracker != nil && err == nil {
		_ = p.tracker.IncrementMessages(ctx, tenantID, 1)
	}

	return err
}
