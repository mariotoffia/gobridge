package runtime

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultCredentialCacheTTL is the default TTL for cached credentials.
const DefaultCredentialCacheTTL = 5 * time.Minute

var _ ports.CredentialStore = (*CredentialResolver)(nil)

// CredentialResolverOption configures a CredentialResolver.
type CredentialResolverOption func(*CredentialResolver)

// WithCredentialCacheTTL sets the cache TTL for resolved credentials.
func WithCredentialCacheTTL(ttl time.Duration) CredentialResolverOption {
	return func(r *CredentialResolver) { r.cacheTTL = ttl }
}

// WithCredentialCacheDisabled disables credential caching entirely.
func WithCredentialCacheDisabled() CredentialResolverOption {
	return func(r *CredentialResolver) { r.cacheDisabled = true }
}

// WithCredentialClock sets the clock used for TTL expiry checks.
// Defaults to clock.System. Use clocktest.Fake in tests for determinism.
func WithCredentialClock(clk clock.Clock) CredentialResolverOption {
	return func(r *CredentialResolver) { r.clk = clk }
}

// CredentialResolver implements ports.CredentialStore by dispatching to
// registered CredentialRepository backends based on URI scheme and
// longest namespace prefix match.
type CredentialResolver struct {
	mu            sync.RWMutex
	repos         []ports.CredentialRepository
	cache         map[string]*credCacheEntry
	cacheTTL      time.Duration
	cacheDisabled bool
	clk           clock.Clock
}

type credCacheEntry struct {
	creds     *domain.CredentialSet
	expiresAt time.Time
}

// NewCredentialResolver creates a resolver with the given options.
func NewCredentialResolver(opts ...CredentialResolverOption) *CredentialResolver {
	r := &CredentialResolver{
		repos:    make([]ports.CredentialRepository, 0),
		cache:    make(map[string]*credCacheEntry),
		cacheTTL: DefaultCredentialCacheTTL,
		clk:      clock.System,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds a credential repository backend. Should be called
// during initialization before Resolve calls.
func (r *CredentialResolver) Register(repo ports.CredentialRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repos = append(r.repos, repo)
}

// Resolve looks up credentials for the given URI. It selects the best
// matching repository by scheme and longest namespace prefix, fetches
// credentials from it, and optionally caches the result.
func (r *CredentialResolver) Resolve(ctx context.Context, uri string) (*domain.CredentialSet, error) {
	if !r.cacheDisabled {
		if creds := r.getCached(uri); creds != nil {
			return creds, nil
		}
	}

	repo, err := r.resolveRepo(uri)
	if err != nil {
		return nil, err
	}

	creds, err := repo.Get(ctx, uri)
	if err != nil {
		return nil, err
	}

	if !r.cacheDisabled {
		r.setCached(uri, creds)
		return cloneCredentialSet(creds), nil
	}

	return creds, nil
}

// resolveRepo finds the best matching repository for the URI.
func (r *CredentialResolver) resolveRepo(uri string) (ports.CredentialRepository, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("credential resolver: invalid URI %q: %w", uri, err)
	}

	scheme := u.Scheme
	path := strings.Trim(u.Host+"/"+strings.Trim(u.Path, "/"), "/")

	r.mu.RLock()
	defer r.mu.RUnlock()

	var (
		best    ports.CredentialRepository
		bestLen = -1
	)

	for _, repo := range r.repos {
		if repo.Scheme() != scheme {
			continue
		}
		ns := strings.Trim(repo.Namespace(), "/")
		if ns == "" {
			if best == nil && bestLen < 0 {
				best = repo
				bestLen = 0
			}
			continue
		}
		if path == ns || strings.HasPrefix(path, ns+"/") {
			if len(ns) > bestLen {
				best = repo
				bestLen = len(ns)
			}
		}
	}

	if best == nil {
		return nil, domain.ErrNotFound.WithMessage(
			fmt.Sprintf("no credential repository for URI %q", uri),
		)
	}
	return best, nil
}

func (r *CredentialResolver) getCached(uri string) *domain.CredentialSet {
	r.mu.RLock()
	entry, ok := r.cache[uri]
	r.mu.RUnlock()

	if !ok {
		return nil
	}
	if r.clk.Now().After(entry.expiresAt) {
		r.mu.Lock()
		if current, exists := r.cache[uri]; exists && current == entry {
			delete(r.cache, uri)
		}
		r.mu.Unlock()
		return nil
	}
	return cloneCredentialSet(entry.creds)
}

func cloneCredentialSet(c *domain.CredentialSet) *domain.CredentialSet {
	if c == nil {
		return nil
	}
	cp := *c
	if c.Password != nil {
		p := *c.Password
		cp.Password = &p
	}
	if c.TLS != nil {
		t := *c.TLS
		if c.TLS.CAPEMs != nil {
			t.CAPEMs = make([]string, len(c.TLS.CAPEMs))
			copy(t.CAPEMs, c.TLS.CAPEMs)
		}
		cp.TLS = &t
	}
	return &cp
}

const maxCredentialCacheEntries = 1000

func (r *CredentialResolver) setCached(uri string, creds *domain.CredentialSet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[uri] = &credCacheEntry{
		creds:     creds,
		expiresAt: r.clk.Now().Add(r.cacheTTL),
	}
	if len(r.cache) > maxCredentialCacheEntries {
		now := r.clk.Now()
		for k, e := range r.cache {
			if now.After(e.expiresAt) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) > maxCredentialCacheEntries {
			r.evictOldestBatch()
		}
	}
}

// evictOldestBatch removes approximately 10% of the oldest entries (by expiry
// time) to provide headroom under burst traffic. The caller must hold r.mu.Lock().
func (r *CredentialResolver) evictOldestBatch() {
	overflow := len(r.cache) - maxCredentialCacheEntries
	evictCount := maxCredentialCacheEntries / 10 // 10% of max
	if evictCount < overflow {
		evictCount = overflow
	}

	type keyExpiry struct {
		key       string
		expiresAt time.Time
	}

	entries := make([]keyExpiry, 0, len(r.cache))
	for k, e := range r.cache {
		entries = append(entries, keyExpiry{key: k, expiresAt: e.expiresAt})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].expiresAt.Before(entries[j].expiresAt)
	})

	for i := 0; i < evictCount && i < len(entries); i++ {
		delete(r.cache, entries[i].key)
	}
}

// InvalidateCache removes a specific URI from the cache.
func (r *CredentialResolver) InvalidateCache(uri string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, uri)
}

// ClearCache removes all entries from the cache.
func (r *CredentialResolver) ClearCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]*credCacheEntry)
}

// CacheStats returns current cache statistics.
func (r *CredentialResolver) CacheStats() CredentialCacheStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var expired int
	now := r.clk.Now()
	for _, e := range r.cache {
		if now.After(e.expiresAt) {
			expired++
		}
	}
	return CredentialCacheStats{
		Size:    len(r.cache),
		Expired: expired,
		Active:  len(r.cache) - expired,
	}
}

// CredentialCacheStats holds cache statistics.
type CredentialCacheStats struct {
	Size    int
	Expired int
	Active  int
}
