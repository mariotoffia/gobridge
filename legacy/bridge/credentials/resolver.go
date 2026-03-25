package credentials

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// DefaultCacheTTL is the default TTL for cached credentials.
const DefaultCacheTTL = 5 * time.Minute

// ResolverOption is a functional option for configuring the Resolver.
type ResolverOption func(*Resolver)

// WithCacheTTL sets the cache TTL for credentials.
func WithCacheTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) {
		r.cacheTTL = ttl
	}
}

// WithCacheDisabled disables credential caching.
func WithCacheDisabled() ResolverOption {
	return func(r *Resolver) {
		r.cacheDisabled = true
	}
}

type Resolver struct {
	mu            *sync.RWMutex
	registry      []types.CredentialsRepository
	cache         map[string]*cacheEntry
	cacheTTL      time.Duration
	cacheDisabled bool
}

// cacheEntry holds a cached credential with expiry time.
type cacheEntry struct {
	credentials *types.Credentials
	expiresAt   time.Time
}

// NewResolver creates a new Credentials Repository Resolver.
func NewResolver(opts ...ResolverOption) *Resolver {
	r := &Resolver{
		mu:       &sync.RWMutex{},
		registry: make([]types.CredentialsRepository, 0),
		cache:    make(map[string]*cacheEntry),
		cacheTTL: DefaultCacheTTL,
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// RegisterRepository adds a repository to the registry.
// Should be called during initialization (before lookups).
func (r *Resolver) RegisterRepository(repo types.CredentialsRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry = append(r.registry, repo)
}

// ResolveRepository returns the best matching repository for the given serverURI.
//
// Returns (repo, true) if found, or (nil, false) if none matches.
func (r *Resolver) ResolveRepository(serverURI string) (types.CredentialsRepository, bool, error) {
	u, err := url.Parse(serverURI)
	if err != nil {
		return nil, false, fmt.Errorf("invalid server URI %q: %w", serverURI, err)
	}

	scheme := u.Scheme
	// Combine host + path for full namespace comparison
	path := strings.Trim(strings.TrimPrefix(u.Host+"/"+strings.Trim(u.Path, "/"), "/"), "/")

	r.mu.RLock()
	defer r.mu.RUnlock()

	var (
		bestMatch        types.CredentialsRepository
		bestNamespaceLen = -1
	)

	for _, repo := range r.registry {
		if repo.GetScheme() != scheme {
			continue
		}
		ns := strings.Trim(repo.GetNamespace(), "/")
		if ns == "" {
			if bestMatch == nil && bestNamespaceLen < 0 {
				bestMatch = repo
				bestNamespaceLen = 0
			}
			continue
		}
		if path == ns || strings.HasPrefix(path, ns+"/") {
			if len(ns) > bestNamespaceLen {
				bestMatch = repo
				bestNamespaceLen = len(ns)
			}
		}
	}

	if bestMatch == nil {
		return nil, false, nil
	}
	return bestMatch, true, nil
}

// GetCredentials retrieves credentials for the given serverURI.
// If caching is enabled, it returns cached credentials if they haven't expired.
// Otherwise, it resolves the repository and fetches fresh credentials.
func (r *Resolver) GetCredentials(serverURI string) (*types.Credentials, error) {
	// Check cache first (if enabled)
	if !r.cacheDisabled {
		if creds := r.getCached(serverURI); creds != nil {
			return creds, nil
		}
	}

	// Resolve repository
	repo, found, err := r.ResolveRepository(serverURI)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no repository found for URI %q", serverURI)
	}

	// Fetch credentials from repository
	creds, err := repo.GetCredentials(serverURI)
	if err != nil {
		return nil, err
	}

	// Cache the result (if enabled)
	if !r.cacheDisabled {
		r.setCached(serverURI, creds)
	}

	return creds, nil
}

// getCached returns cached credentials if they exist and haven't expired.
func (r *Resolver) getCached(serverURI string) *types.Credentials {
	r.mu.RLock()
	entry, exists := r.cache[serverURI]
	r.mu.RUnlock()

	if !exists {
		return nil
	}

	if time.Now().After(entry.expiresAt) {
		// Expired - remove from cache
		r.mu.Lock()
		delete(r.cache, serverURI)
		r.mu.Unlock()
		return nil
	}

	return entry.credentials
}

// setCached stores credentials in the cache.
func (r *Resolver) setCached(serverURI string, creds *types.Credentials) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache[serverURI] = &cacheEntry{
		credentials: creds,
		expiresAt:   time.Now().Add(r.cacheTTL),
	}
}

// InvalidateCache removes a specific URI from the cache.
func (r *Resolver) InvalidateCache(serverURI string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, serverURI)
}

// ClearCache removes all entries from the cache.
func (r *Resolver) ClearCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]*cacheEntry)
}

// CacheStats returns current cache statistics.
func (r *Resolver) CacheStats() CacheStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var expired int
	now := time.Now()
	for _, entry := range r.cache {
		if now.After(entry.expiresAt) {
			expired++
		}
	}

	return CacheStats{
		Size:    len(r.cache),
		Expired: expired,
		Active:  len(r.cache) - expired,
	}
}

// CacheStats holds cache statistics.
type CacheStats struct {
	// Size is the total number of entries in the cache.
	Size int
	// Expired is the number of expired entries.
	Expired int
	// Active is the number of non-expired entries.
	Active int
}
