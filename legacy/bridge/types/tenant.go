// Package types provides multi-tenancy types and interfaces.
package types

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ============================================================================
// Tenant Errors
// ============================================================================

var (
	// ErrTenantNotFound is returned when a tenant is not found.
	ErrTenantNotFound = errors.New("tenant not found")
	// ErrTenantDisabled is returned when a tenant is disabled.
	ErrTenantDisabled = errors.New("tenant is disabled")
	// ErrTenantQuotaExceeded is returned when a tenant's quota is exceeded.
	ErrTenantQuotaExceeded = errors.New("tenant quota exceeded")
	// ErrTenantRateLimited is returned when a tenant is rate limited.
	ErrTenantRateLimited = errors.New("tenant rate limited")
)

// ============================================================================
// Tenant Context Key
// ============================================================================

// tenantContextKey is the context key for tenant information.
type tenantContextKey struct{}

// TenantFromContext retrieves the tenant from context.
func TenantFromContext(ctx context.Context) (*Tenant, bool) {
	t, ok := ctx.Value(tenantContextKey{}).(*Tenant)
	return t, ok
}

// ContextWithTenant adds a tenant to the context.
func ContextWithTenant(ctx context.Context, tenant *Tenant) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

// TenantIDFromContext retrieves just the tenant ID from context.
func TenantIDFromContext(ctx context.Context) string {
	if t, ok := TenantFromContext(ctx); ok {
		return t.ID
	}
	return ""
}

// ============================================================================
// Tenant
// ============================================================================

// Tenant represents a tenant in a multi-tenant bridge deployment.
type Tenant struct {
	// ID is the unique tenant identifier.
	ID string `json:"id"`
	// Name is the human-readable tenant name.
	Name string `json:"name"`
	// Enabled indicates if the tenant is active.
	Enabled bool `json:"enabled"`
	// Metadata contains tenant-specific metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Config contains tenant-specific configuration overrides.
	Config *TenantConfig `json:"config,omitempty"`
	// Quotas defines tenant resource limits.
	Quotas *TenantQuotas `json:"quotas,omitempty"`
	// CreatedAt is when the tenant was created.
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is when the tenant was last updated.
	UpdatedAt time.Time `json:"updatedAt"`
}

// TenantConfig contains tenant-specific configuration overrides.
type TenantConfig struct {
	// TransportRetry overrides the default transport retry config.
	TransportRetry *TransportRetryConfig `json:"transportRetry,omitempty"`
	// FlowControl overrides the default flow control config.
	FlowControl *FlowControlConfig `json:"flowControl,omitempty"`
	// CircuitBreaker overrides the default circuit breaker config.
	CircuitBreaker *CircuitBreakerConfig `json:"circuitBreaker,omitempty"`
	// AllowedConnections restricts which connections this tenant can use.
	// Empty means all connections are allowed.
	AllowedConnections []string `json:"allowedConnections,omitempty"`
	// AllowedTopics restricts which topics this tenant can access.
	// Supports wildcards: "tenant1/*", "sensors/#"
	AllowedTopics []string `json:"allowedTopics,omitempty"`
}

// TenantQuotas defines resource limits for a tenant.
type TenantQuotas struct {
	// MaxPipelines is the maximum number of pipelines.
	MaxPipelines int `json:"maxPipelines,omitempty"`
	// MaxConnections is the maximum number of connections.
	MaxConnections int `json:"maxConnections,omitempty"`
	// MaxMessagesPerSecond is the rate limit for messages.
	MaxMessagesPerSecond int `json:"maxMessagesPerSecond,omitempty"`
	// MaxMessageSizeBytes is the maximum message size.
	MaxMessageSizeBytes int64 `json:"maxMessageSizeBytes,omitempty"`
	// MaxInFlightMessages is the maximum in-flight messages.
	MaxInFlightMessages int `json:"maxInFlightMessages,omitempty"`
	// MaxDailyMessages is the maximum messages per day.
	MaxDailyMessages int64 `json:"maxDailyMessages,omitempty"`
	// MaxMonthlyMessages is the maximum messages per month.
	MaxMonthlyMessages int64 `json:"maxMonthlyMessages,omitempty"`
}

// ============================================================================
// Tenant Usage Tracking
// ============================================================================

// TenantUsage tracks a tenant's resource usage.
type TenantUsage struct {
	// TenantID is the tenant identifier.
	TenantID string `json:"tenantId"`
	// PipelineCount is the current number of pipelines.
	PipelineCount int `json:"pipelineCount"`
	// ConnectionCount is the current number of connections.
	ConnectionCount int `json:"connectionCount"`
	// MessagesProcessed is the total messages processed.
	MessagesProcessed int64 `json:"messagesProcessed"`
	// MessagesThisSecond is the current second's message count.
	MessagesThisSecond int64 `json:"messagesThisSecond"`
	// MessagesToday is today's message count.
	MessagesToday int64 `json:"messagesToday"`
	// MessagesThisMonth is this month's message count.
	MessagesThisMonth int64 `json:"messagesThisMonth"`
	// LastMessageAt is when the last message was processed.
	LastMessageAt time.Time `json:"lastMessageAt,omitempty"`
	// InFlightMessages is the current in-flight count.
	InFlightMessages int64 `json:"inFlightMessages"`
}

// ============================================================================
// Tenant Repository Interface
// ============================================================================

// TenantRepository manages tenant data.
type TenantRepository interface {
	// Get retrieves a tenant by ID.
	Get(ctx context.Context, id string) (*Tenant, error)
	// List returns all tenants.
	List(ctx context.Context) ([]*Tenant, error)
	// Create creates a new tenant.
	Create(ctx context.Context, tenant *Tenant) error
	// Update updates an existing tenant.
	Update(ctx context.Context, tenant *Tenant) error
	// Delete removes a tenant.
	Delete(ctx context.Context, id string) error
	// Exists checks if a tenant exists.
	Exists(ctx context.Context, id string) (bool, error)
}

// TenantUsageRepository tracks tenant usage.
type TenantUsageRepository interface {
	// GetUsage returns current usage for a tenant.
	GetUsage(ctx context.Context, tenantID string) (*TenantUsage, error)
	// IncrementMessages increments the message count.
	IncrementMessages(ctx context.Context, tenantID string, count int64) error
	// IncrementInFlight changes the in-flight count.
	IncrementInFlight(ctx context.Context, tenantID string, delta int64) error
	// ResetDailyCounters resets daily counters (call at midnight).
	ResetDailyCounters(ctx context.Context) error
	// ResetMonthlyCounters resets monthly counters (call at month start).
	ResetMonthlyCounters(ctx context.Context) error
}

// ============================================================================
// Tenant Manager
// ============================================================================

// TenantManager provides tenant management with caching and validation.
type TenantManager struct {
	repo      TenantRepository
	usageRepo TenantUsageRepository
	cache     sync.Map // map[string]*tenantCacheEntry
	cacheTTL  time.Duration
}

type tenantCacheEntry struct {
	tenant    *Tenant
	expiresAt time.Time
}

// TenantManagerConfig configures the tenant manager.
type TenantManagerConfig struct {
	// CacheTTL is how long to cache tenant data.
	CacheTTL time.Duration `json:"cacheTTL,omitempty"`
}

// NewTenantManager creates a new tenant manager.
func NewTenantManager(repo TenantRepository, usageRepo TenantUsageRepository, config TenantManagerConfig) *TenantManager {
	if config.CacheTTL <= 0 {
		config.CacheTTL = 5 * time.Minute
	}
	return &TenantManager{
		repo:      repo,
		usageRepo: usageRepo,
		cacheTTL:  config.CacheTTL,
	}
}

// Get retrieves a tenant by ID with caching.
func (m *TenantManager) Get(ctx context.Context, id string) (*Tenant, error) {
	// Check cache
	if entry, ok := m.cache.Load(id); ok {
		e := entry.(*tenantCacheEntry)
		if time.Now().Before(e.expiresAt) {
			return e.tenant, nil
		}
		m.cache.Delete(id)
	}

	// Load from repository
	tenant, err := m.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// Cache the result
	m.cache.Store(id, &tenantCacheEntry{
		tenant:    tenant,
		expiresAt: time.Now().Add(m.cacheTTL),
	})

	return tenant, nil
}

// Validate checks if a tenant can process a message.
func (m *TenantManager) Validate(ctx context.Context, tenantID string) error {
	tenant, err := m.Get(ctx, tenantID)
	if err != nil {
		return err
	}

	if !tenant.Enabled {
		return ErrTenantDisabled
	}

	// Check quotas if usage repo is available
	if m.usageRepo != nil && tenant.Quotas != nil {
		usage, err := m.usageRepo.GetUsage(ctx, tenantID)
		if err != nil {
			return err
		}

		if err := m.checkQuotas(tenant.Quotas, usage); err != nil {
			return err
		}
	}

	return nil
}

// checkQuotas validates usage against quotas.
func (m *TenantManager) checkQuotas(quotas *TenantQuotas, usage *TenantUsage) error {
	if quotas.MaxMessagesPerSecond > 0 && usage.MessagesThisSecond >= int64(quotas.MaxMessagesPerSecond) {
		return ErrTenantRateLimited
	}
	if quotas.MaxDailyMessages > 0 && usage.MessagesToday >= quotas.MaxDailyMessages {
		return ErrTenantQuotaExceeded
	}
	if quotas.MaxMonthlyMessages > 0 && usage.MessagesThisMonth >= quotas.MaxMonthlyMessages {
		return ErrTenantQuotaExceeded
	}
	if quotas.MaxInFlightMessages > 0 && usage.InFlightMessages >= int64(quotas.MaxInFlightMessages) {
		return ErrTenantQuotaExceeded
	}
	return nil
}

// InvalidateCache removes a tenant from the cache.
func (m *TenantManager) InvalidateCache(id string) {
	m.cache.Delete(id)
}

// Create creates a new tenant.
func (m *TenantManager) Create(ctx context.Context, tenant *Tenant) error {
	tenant.CreatedAt = time.Now()
	tenant.UpdatedAt = time.Now()
	return m.repo.Create(ctx, tenant)
}

// Update updates a tenant and invalidates cache.
func (m *TenantManager) Update(ctx context.Context, tenant *Tenant) error {
	tenant.UpdatedAt = time.Now()
	if err := m.repo.Update(ctx, tenant); err != nil {
		return err
	}
	m.InvalidateCache(tenant.ID)
	return nil
}

// Delete removes a tenant and invalidates cache.
func (m *TenantManager) Delete(ctx context.Context, id string) error {
	if err := m.repo.Delete(ctx, id); err != nil {
		return err
	}
	m.InvalidateCache(id)
	return nil
}

// List returns all tenants.
func (m *TenantManager) List(ctx context.Context) ([]*Tenant, error) {
	return m.repo.List(ctx)
}

// ============================================================================
// In-Memory Tenant Repository
// ============================================================================

// InMemoryTenantRepository is a simple in-memory tenant repository.
type InMemoryTenantRepository struct {
	mu      sync.RWMutex
	tenants map[string]*Tenant
}

// NewInMemoryTenantRepository creates a new in-memory tenant repository.
func NewInMemoryTenantRepository() *InMemoryTenantRepository {
	return &InMemoryTenantRepository{
		tenants: make(map[string]*Tenant),
	}
}

// Get retrieves a tenant by ID.
func (r *InMemoryTenantRepository) Get(ctx context.Context, id string) (*Tenant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tenant, ok := r.tenants[id]
	if !ok {
		return nil, ErrTenantNotFound
	}
	return tenant, nil
}

// List returns all tenants.
func (r *InMemoryTenantRepository) List(ctx context.Context) ([]*Tenant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		result = append(result, t)
	}
	return result, nil
}

// Create creates a new tenant.
func (r *InMemoryTenantRepository) Create(ctx context.Context, tenant *Tenant) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tenants[tenant.ID]; exists {
		return ErrAlreadyExists
	}
	r.tenants[tenant.ID] = tenant
	return nil
}

// Update updates an existing tenant.
func (r *InMemoryTenantRepository) Update(ctx context.Context, tenant *Tenant) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tenants[tenant.ID]; !exists {
		return ErrTenantNotFound
	}
	r.tenants[tenant.ID] = tenant
	return nil
}

// Delete removes a tenant.
func (r *InMemoryTenantRepository) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tenants[id]; !exists {
		return ErrTenantNotFound
	}
	delete(r.tenants, id)
	return nil
}

// Exists checks if a tenant exists.
func (r *InMemoryTenantRepository) Exists(ctx context.Context, id string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.tenants[id]
	return exists, nil
}

// Ensure InMemoryTenantRepository implements TenantRepository.
var _ TenantRepository = (*InMemoryTenantRepository)(nil)

// ============================================================================
// In-Memory Tenant Usage Repository
// ============================================================================

// InMemoryTenantUsageRepository is a simple in-memory usage tracker.
type InMemoryTenantUsageRepository struct {
	mu    sync.RWMutex
	usage map[string]*TenantUsage
}

// NewInMemoryTenantUsageRepository creates a new in-memory usage repository.
func NewInMemoryTenantUsageRepository() *InMemoryTenantUsageRepository {
	return &InMemoryTenantUsageRepository{
		usage: make(map[string]*TenantUsage),
	}
}

// GetUsage returns current usage for a tenant.
func (r *InMemoryTenantUsageRepository) GetUsage(ctx context.Context, tenantID string) (*TenantUsage, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	usage, ok := r.usage[tenantID]
	if !ok {
		return &TenantUsage{TenantID: tenantID}, nil
	}
	return usage, nil
}

// IncrementMessages increments the message count.
func (r *InMemoryTenantUsageRepository) IncrementMessages(ctx context.Context, tenantID string, count int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	usage, ok := r.usage[tenantID]
	if !ok {
		usage = &TenantUsage{TenantID: tenantID}
		r.usage[tenantID] = usage
	}

	usage.MessagesProcessed += count
	usage.MessagesThisSecond += count
	usage.MessagesToday += count
	usage.MessagesThisMonth += count
	usage.LastMessageAt = time.Now()

	return nil
}

// IncrementInFlight changes the in-flight count.
func (r *InMemoryTenantUsageRepository) IncrementInFlight(ctx context.Context, tenantID string, delta int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	usage, ok := r.usage[tenantID]
	if !ok {
		usage = &TenantUsage{TenantID: tenantID}
		r.usage[tenantID] = usage
	}

	usage.InFlightMessages += delta
	if usage.InFlightMessages < 0 {
		usage.InFlightMessages = 0
	}

	return nil
}

// ResetDailyCounters resets daily counters.
func (r *InMemoryTenantUsageRepository) ResetDailyCounters(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, usage := range r.usage {
		usage.MessagesToday = 0
	}
	return nil
}

// ResetMonthlyCounters resets monthly counters.
func (r *InMemoryTenantUsageRepository) ResetMonthlyCounters(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, usage := range r.usage {
		usage.MessagesThisMonth = 0
	}
	return nil
}

// Ensure InMemoryTenantUsageRepository implements TenantUsageRepository.
var _ TenantUsageRepository = (*InMemoryTenantUsageRepository)(nil)

// ============================================================================
// Tenant Middleware Helper
// ============================================================================

// TenantExtractor extracts tenant ID from a message.
type TenantExtractor func(msg *Message) string

// DefaultTenantExtractor extracts tenant from message metadata.
func DefaultTenantExtractor(msg *Message) string {
	if msg.Metadata == nil {
		return ""
	}
	if tenantID, ok := msg.Metadata["tenantId"].(string); ok {
		return tenantID
	}
	if tenantID, ok := msg.Metadata["tenant_id"].(string); ok {
		return tenantID
	}
	return ""
}

// TopicTenantExtractor extracts tenant from topic prefix.
// Assumes format: "tenants/{tenant_id}/..."
func TopicTenantExtractor(msg *Message) string {
	if len(msg.Topic) < 9 { // "tenants/x"
		return ""
	}
	if msg.Topic[:8] != "tenants/" {
		return ""
	}
	// Find next slash
	for i := 8; i < len(msg.Topic); i++ {
		if msg.Topic[i] == '/' {
			return msg.Topic[8:i]
		}
	}
	return msg.Topic[8:]
}
