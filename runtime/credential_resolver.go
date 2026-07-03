package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	return func(r *CredentialResolver) {
		if clk != nil {
			r.clk = clk
		}
	}
}

// CredentialResolver implements ports.CredentialStore by dispatching to
// registered CredentialRepository backends based on URI scheme and
// longest namespace prefix match.
//
// Cache-miss fetches are deduplicated per URI (singleflight): under a
// thundering herd of concurrent Resolve calls for the same URI only one
// repository Get is issued; the rest wait for its result.
type CredentialResolver struct {
	mu            sync.RWMutex
	repos         []ports.CredentialRepository
	cache         map[string]*credCacheEntry
	cacheTTL      time.Duration
	cacheDisabled bool
	clk           clock.Clock

	// flightMu guards flight, the in-progress cache-miss fetches.
	// Separate from mu because repository Gets block on I/O and must
	// not hold the cache lock.
	flightMu sync.Mutex
	flight   map[string]*credFlight
}

type credCacheEntry struct {
	creds     *connectivity.CredentialSet
	expiresAt time.Time
}

// credFlight is one in-progress fetch shared by concurrent Resolve
// callers for the same URI. done is closed once creds/err are set.
type credFlight struct {
	done  chan struct{}
	creds *connectivity.CredentialSet
	err   error
}

// NewCredentialResolver creates a resolver with the given options.
func NewCredentialResolver(opts ...CredentialResolverOption) *CredentialResolver {
	r := &CredentialResolver{
		repos:    make([]ports.CredentialRepository, 0),
		cache:    make(map[string]*credCacheEntry),
		cacheTTL: DefaultCredentialCacheTTL,
		clk:      clock.System,
		flight:   make(map[string]*credFlight),
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
// credentials from it, and optionally caches the result. Concurrent
// cache misses for the same URI are coalesced into a single repository
// fetch.
func (r *CredentialResolver) Resolve(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	if r.cacheDisabled {
		return r.fetch(ctx, uri)
	}

	// Retry loop for the singleflight join: a follower must not inherit the
	// LEADER's context cancellation. If the leader's fetch failed purely
	// because the leader's own ctx died while this caller's ctx is still
	// healthy, we loop back — re-check the cache, then either join a newer
	// flight or become the new leader — instead of returning the leader's
	// spurious cancellation (finding: singleflight leader-ctx propagation).
	for {
		if creds := r.getCached(uri); creds != nil {
			return creds, nil
		}

		// This caller's own context is already dead: fail with OUR error
		// rather than registering or joining a flight.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Singleflight: join an in-progress fetch for this URI or become
		// its leader.
		r.flightMu.Lock()
		if fl, ok := r.flight[uri]; ok {
			r.flightMu.Unlock()
			select {
			case <-fl.done:
				if fl.err != nil {
					// The leader failed. Only a CONTEXT error that belongs to
					// the leader (not us) is retryable: our ctx is healthy, so
					// re-run the loop and lead/join a fresh flight. Every other
					// error is a genuine resolution failure and propagates to
					// followers unchanged (deliberate).
					if isContextErr(fl.err) && ctx.Err() == nil {
						continue
					}
					return nil, fl.err
				}
				return cloneCredentialSet(fl.creds), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		fl := &credFlight{done: make(chan struct{})}
		r.flight[uri] = fl
		r.flightMu.Unlock()

		creds, err := r.fetch(ctx, uri)
		fl.creds, fl.err = creds, err

		r.flightMu.Lock()
		delete(r.flight, uri)
		r.flightMu.Unlock()
		close(fl.done)

		if err != nil {
			return nil, err
		}
		return cloneCredentialSet(creds), nil
	}
}

// isContextErr reports whether err is (or wraps) a context cancellation or
// deadline error. Used to distinguish a leader's own-ctx failure — which a
// healthy follower must not inherit — from a genuine resolution error.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ResolveUncached bypasses the read side of the cache and fetches fresh
// credentials directly from the backing repository, then refreshes the
// cache with the fresh value. It implements the poll wrapper's
// UncachedPullCredentialStore capability (runtime/credentials): the
// rotation poll must observe the store, not the cache — otherwise a
// 30s rotation poll against a 5m cache TTL detects rotations only at
// TTL expiry. Refreshing (rather than just bypassing) the cache means a
// connection rebuild that happens right after a rotation resolves the
// rotated secret instead of the rotated-out one.
func (r *CredentialResolver) ResolveUncached(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	return r.fetch(ctx, uri)
}

// fetch resolves the repository, gets the credentials and refreshes the
// cache (unless caching is disabled). Callers own singleflight; fetch
// itself performs exactly one repository Get.
func (r *CredentialResolver) fetch(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
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
		return nil, shared.ErrNotFound.WithMessage(
			fmt.Sprintf("no credential repository for URI %q", uri),
		)
	}
	return best, nil
}

func (r *CredentialResolver) getCached(uri string) *connectivity.CredentialSet {
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

func cloneCredentialSet(c *connectivity.CredentialSet) *connectivity.CredentialSet {
	if c == nil {
		return nil
	}
	// PasswordCredential and TLSMaterial are immutable value objects, so the
	// cached pointers can be shared safely. A fresh CredentialSet wrapper is
	// still returned so the caller holds an independent container that cannot
	// corrupt the cached entry.
	return connectivity.NewCredentialSet(c.Password(), c.TLS())
}

const maxCredentialCacheEntries = 1000

func (r *CredentialResolver) setCached(uri string, creds *connectivity.CredentialSet) {
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
