// Package circuitbreaker provides middleware for circuit breaker protection.
//
// This middleware wraps pipeline processing with circuit breaker logic:
// - Tracks failures and opens circuit when threshold is reached
// - Rejects requests when circuit is open
// - Tests recovery with limited requests in half-open state
// - Supports per-target, per-tenant, or global circuit breakers
//
// # Usage
//
//	cbMW := circuitbreaker.NewMiddleware(
//	    circuitbreaker.WithConfig(types.DefaultCircuitBreakerConfig()),
//	    circuitbreaker.WithKeyExtractor(circuitbreaker.TargetKey),
//	)
//
//	pipeline := core.NewPipeline("my-pipeline", source, target, cbMW)
package circuitbreaker

import (
	"context"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// KeyExtractor extracts a key for the circuit breaker from context/message.
// Different messages can use different circuit breakers based on this key.
type KeyExtractor func(ctx context.Context, msg *types.Message) string

// GlobalKey always returns the same key (one circuit breaker for all).
func GlobalKey(_ context.Context, _ *types.Message) string {
	return "global"
}

// TopicKey uses the message topic as the key.
func TopicKey(_ context.Context, msg *types.Message) string {
	return msg.Topic
}

// TenantKey uses the tenant ID as the key.
func TenantKey(ctx context.Context, _ *types.Message) string {
	if tenant, ok := types.TenantFromContext(ctx); ok {
		return tenant.ID
	}
	return "default"
}

// Middleware provides circuit breaker protection for message processing.
type Middleware struct {
	name         string
	registry     *types.CircuitBreakerRegistry
	keyExtractor KeyExtractor
}

// Option configures the middleware.
type Option func(*Middleware)

// WithName sets the middleware name.
func WithName(name string) Option {
	return func(m *Middleware) {
		m.name = name
	}
}

// WithRegistry sets a custom circuit breaker registry.
func WithRegistry(registry *types.CircuitBreakerRegistry) Option {
	return func(m *Middleware) {
		m.registry = registry
	}
}

// WithConfig sets the default circuit breaker configuration.
func WithConfig(config types.CircuitBreakerConfig) Option {
	return func(m *Middleware) {
		m.registry = types.NewCircuitBreakerRegistry(config)
	}
}

// WithKeyExtractor sets the key extractor function.
func WithKeyExtractor(extractor KeyExtractor) Option {
	return func(m *Middleware) {
		m.keyExtractor = extractor
	}
}

// NewMiddleware creates a new circuit breaker middleware.
func NewMiddleware(opts ...Option) *Middleware {
	m := &Middleware{
		name:         "circuitbreaker",
		registry:     types.NewCircuitBreakerRegistry(types.DefaultCircuitBreakerConfig()),
		keyExtractor: GlobalKey,
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

// Process wraps message processing with circuit breaker protection.
func (m *Middleware) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
	// Get circuit breaker key
	key := m.keyExtractor(ctx, msg)

	// Get or create circuit breaker for this key
	cb := m.registry.Get(key)

	// Execute with circuit breaker protection
	return cb.Execute(ctx, func(ctx context.Context) error {
		return next(ctx, msg)
	})
}

// Registry returns the circuit breaker registry.
func (m *Middleware) Registry() *types.CircuitBreakerRegistry {
	return m.registry
}

// GetCircuitBreaker returns the circuit breaker for a key.
func (m *Middleware) GetCircuitBreaker(key string) types.CircuitBreaker {
	return m.registry.Get(key)
}

// Metrics returns metrics for all circuit breakers.
func (m *Middleware) Metrics() []types.CircuitBreakerMetrics {
	return m.registry.Metrics()
}

// Ensure Middleware implements types.Middleware.
var _ types.Middleware = (*Middleware)(nil)
