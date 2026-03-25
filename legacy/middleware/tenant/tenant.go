// Package tenant provides middleware for multi-tenant message processing.
//
// This middleware:
// - Extracts tenant ID from messages
// - Validates tenant is active and within quotas
// - Adds tenant context for downstream processing
// - Tracks per-tenant usage and metrics
//
// # Usage
//
//	tenantMW := tenant.NewMiddleware(tenantManager,
//	    tenant.WithExtractor(types.DefaultTenantExtractor),
//	    tenant.WithRequireTenant(true),
//	)
//
//	pipeline := core.NewPipeline("my-pipeline", source, target, tenantMW)
package tenant

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Middleware validates and tracks tenant information for messages.
type Middleware struct {
	name          string
	manager       *types.TenantManager
	extractor     types.TenantExtractor
	requireTenant bool
	usageRepo     types.TenantUsageRepository
}

// Option configures the middleware.
type Option func(*Middleware)

// WithName sets the middleware name.
func WithName(name string) Option {
	return func(m *Middleware) {
		m.name = name
	}
}

// WithExtractor sets the tenant ID extractor.
func WithExtractor(extractor types.TenantExtractor) Option {
	return func(m *Middleware) {
		m.extractor = extractor
	}
}

// WithRequireTenant sets whether tenant ID is required.
func WithRequireTenant(require bool) Option {
	return func(m *Middleware) {
		m.requireTenant = require
	}
}

// WithUsageTracking enables usage tracking.
func WithUsageTracking(repo types.TenantUsageRepository) Option {
	return func(m *Middleware) {
		m.usageRepo = repo
	}
}

// NewMiddleware creates a new tenant middleware.
func NewMiddleware(manager *types.TenantManager, opts ...Option) *Middleware {
	m := &Middleware{
		name:          "tenant",
		manager:       manager,
		extractor:     types.DefaultTenantExtractor,
		requireTenant: false,
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Name returns the middleware name.
func (m *Middleware) Name() string {
	return m.name
}

// Process validates the tenant and adds context.
func (m *Middleware) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
	// Extract tenant ID
	tenantID := m.extractor(msg)

	if tenantID == "" {
		if m.requireTenant {
			return fmt.Errorf("tenant ID required but not found in message")
		}
		// No tenant, pass through
		return next(ctx, msg)
	}

	// Validate tenant
	if err := m.manager.Validate(ctx, tenantID); err != nil {
		return fmt.Errorf("tenant validation failed: %w", err)
	}

	// Get tenant for context
	tenant, err := m.manager.Get(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("failed to get tenant: %w", err)
	}

	// Check message size quota
	if tenant.Quotas != nil && tenant.Quotas.MaxMessageSizeBytes > 0 {
		if int64(len(msg.Payload)) > tenant.Quotas.MaxMessageSizeBytes {
			return fmt.Errorf("message size %d exceeds tenant limit %d",
				len(msg.Payload), tenant.Quotas.MaxMessageSizeBytes)
		}
	}

	// Track in-flight
	if m.usageRepo != nil {
		if err := m.usageRepo.IncrementInFlight(ctx, tenantID, 1); err != nil {
			// Log but don't fail
		}
		defer func() {
			m.usageRepo.IncrementInFlight(ctx, tenantID, -1)
		}()
	}

	// Add tenant to context
	ctx = types.ContextWithTenant(ctx, tenant)

	// Add tenant ID to message metadata for downstream
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata["tenantId"] = tenantID

	// Process message
	err = next(ctx, msg)

	// Track usage
	if m.usageRepo != nil && err == nil {
		m.usageRepo.IncrementMessages(ctx, tenantID, 1)
	}

	return err
}

// Ensure Middleware implements types.Middleware.
var _ types.Middleware = (*Middleware)(nil)
