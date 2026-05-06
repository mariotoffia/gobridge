package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type stubRepo struct {
	scheme    string
	namespace string
	creds     *connectivity.CredentialSet
	callCount atomic.Int32
}

func (s *stubRepo) Scheme() string    { return s.scheme }
func (s *stubRepo) Namespace() string { return s.namespace }
func (s *stubRepo) Get(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	s.callCount.Add(1)
	return s.creds, nil
}

// Verifies Resolve dispatches to the registered repository for the URI scheme and returns credentials.
func TestCredentialResolver_SingleSchemeDispatch(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver()
	r.Register(repo)

	got, err := r.Resolve(context.Background(), "file://path/to/creds")
	require.NoError(t, err)
	assert.Equal(t, creds, got)
	assert.Equal(t, int32(1), repo.callCount.Load())
}

// Verifies Resolve selects the correct repository per scheme without cross-calling other schemes.
func TestCredentialResolver_MultiSchemeDispatch(t *testing.T) {
	fileCreds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "file-user", Password: "fp"},
	}
	pmsCreds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "pms-user", Password: "pp"},
	}
	fileRepo := &stubRepo{scheme: "file", creds: fileCreds}
	pmsRepo := &stubRepo{scheme: "pms", creds: pmsCreds}

	r := NewCredentialResolver()
	r.Register(fileRepo)
	r.Register(pmsRepo)

	got, err := r.Resolve(context.Background(), "file://some/path")
	require.NoError(t, err)
	assert.Equal(t, fileCreds, got)
	assert.Equal(t, int32(1), fileRepo.callCount.Load())
	assert.Equal(t, int32(0), pmsRepo.callCount.Load())

	got, err = r.Resolve(context.Background(), "pms://other/path")
	require.NoError(t, err)
	assert.Equal(t, pmsCreds, got)
	assert.Equal(t, int32(1), pmsRepo.callCount.Load())
}

// Verifies namespace matching picks the longest registered prefix for the same scheme.
func TestCredentialResolver_NamespaceLongestPrefix(t *testing.T) {
	rootCreds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "root"},
	}
	tenantACreds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "tenantA"},
	}
	app1Creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "app1"},
	}

	rootRepo := &stubRepo{scheme: "pms", namespace: "", creds: rootCreds}
	tenantARepo := &stubRepo{scheme: "pms", namespace: "tenantA", creds: tenantACreds}
	app1Repo := &stubRepo{scheme: "pms", namespace: "tenantA/app1", creds: app1Creds}

	r := NewCredentialResolver()
	r.Register(rootRepo)
	r.Register(tenantARepo)
	r.Register(app1Repo)

	tests := []struct {
		name     string
		uri      string
		wantUser string
		wantRepo *stubRepo
	}{
		{
			name:     "deepest match tenantA/app1",
			uri:      "pms://tenantA/app1/prod/db",
			wantUser: "app1",
			wantRepo: app1Repo,
		},
		{
			name:     "mid match tenantA",
			uri:      "pms://tenantA/app2/prod/db",
			wantUser: "tenantA",
			wantRepo: tenantARepo,
		},
		{
			name:     "fallback to root",
			uri:      "pms://tenantB/appX/xyz",
			wantUser: "root",
			wantRepo: rootRepo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), tt.uri)
			require.NoError(t, err)
			assert.Equal(t, tt.wantUser, got.Password.Username)
		})
	}
}

// Verifies Resolve returns ErrNotFound when no repository is registered for the URI scheme.
func TestCredentialResolver_NotFoundError(t *testing.T) {
	r := NewCredentialResolver()

	_, err := r.Resolve(context.Background(), "vault://secret/data")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// Verifies a second Resolve for the same URI with TTL cache hits the store only once.
func TestCredentialResolver_CacheHitMiss(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "cached"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	got1, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, creds, got1)

	got2, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, creds, got2)

	assert.Equal(t, int32(1), repo.callCount.Load(), "second call should be served from cache")
}

// Verifies after cache TTL elapses Resolve fetches credentials from the repository again.
func TestCredentialResolver_CacheExpiry(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "expiring"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	const ttl = 10 * time.Millisecond
	clk := clocktest.New()
	r := NewCredentialResolver(WithCredentialCacheTTL(ttl), WithCredentialClock(clk))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(1), repo.callCount.Load())

	clk.Advance(ttl + time.Nanosecond)

	_, err = r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(2), repo.callCount.Load(), "expired cache entry should cause re-fetch")
}

// Verifies WithCredentialCacheDisabled causes every Resolve to call the underlying repository.
func TestCredentialResolver_CacheDisabled(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "nocache"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheDisabled())
	r.Register(repo)

	for i := 0; i < 3; i++ {
		_, err := r.Resolve(context.Background(), "file://data")
		require.NoError(t, err)
	}

	assert.Equal(t, int32(3), repo.callCount.Load(), "all calls should hit the repo directly")
}

// Verifies InvalidateCache forces the next Resolve for that URI to refetch from the repository.
func TestCredentialResolver_InvalidateCache(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "inv"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(1), repo.callCount.Load())

	r.InvalidateCache("file://data")

	_, err = r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(2), repo.callCount.Load(), "invalidated entry should cause re-fetch")
}

// Verifies ClearCache empties all cached entries as reflected by cache stats size.
func TestCredentialResolver_ClearCache(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "clear"},
	}
	repo := &stubRepo{scheme: "pms", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	uris := []string{"pms://a/x", "pms://a/y", "pms://a/z"}
	for _, uri := range uris {
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, 3, r.CacheStats().Size)

	r.ClearCache()
	assert.Equal(t, 0, r.CacheStats().Size)
}

// Verifies the credential cache never holds more than maxCredentialCacheEntries (1000) after many distinct URIs.
func TestCredentialResolver_CacheMaxSize(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	for i := range 1005 {
		uri := fmt.Sprintf("file://entry-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
		stats := r.CacheStats()
		assert.LessOrEqual(t, stats.Size, maxCredentialCacheEntries,
			"cache size after resolve %d", i)
	}
	assert.LessOrEqual(t, r.CacheStats().Size, maxCredentialCacheEntries,
		"cache size should be at or below max after all insertions")
}

// Verifies when over capacity, expired entries are evicted before LRU-by-expiry eviction runs.
func TestCredentialResolver_CacheEvictsExpired(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	const ttl = time.Millisecond
	clk := clocktest.New()
	r := NewCredentialResolver(WithCredentialCacheTTL(ttl), WithCredentialClock(clk))
	r.Register(repo)

	for i := range maxCredentialCacheEntries {
		uri := fmt.Sprintf("file://old-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	clk.Advance(ttl + time.Nanosecond)

	_, err := r.Resolve(context.Background(), "file://fresh-after-expiry")
	require.NoError(t, err)

	stats := r.CacheStats()
	assert.Equal(t, 1, stats.Size, "expired bulk should be dropped when inserting past max size")
	assert.Equal(t, 0, stats.Expired, "remaining entry should be active")
	assert.Equal(t, int32(maxCredentialCacheEntries+1), repo.callCount.Load())
}

// Verifies when still over capacity after removing expired entries, a batch
// of ~10% oldest entries (by expiry) is evicted, providing headroom.
func TestCredentialResolver_CacheEvictsOldestWhenFull(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	// Insert the first entry which will have the earliest expiry.
	firstURI := "file://first-evict-candidate"
	_, err := r.Resolve(context.Background(), firstURI)
	require.NoError(t, err)

	// Fill cache to capacity.
	for i := 1; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://fill-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// One more insert triggers batch eviction of ~10% oldest entries.
	overflowURI := "file://overflow"
	_, err = r.Resolve(context.Background(), overflowURI)
	require.NoError(t, err)

	// After batch eviction: cache should have headroom (well below max).
	stats := r.CacheStats()
	expectedMax := maxCredentialCacheEntries - maxCredentialCacheEntries/10 + 1
	assert.LessOrEqual(t, stats.Size, expectedMax,
		"batch eviction should remove ~10%% of entries, leaving headroom")

	// The first URI (oldest expiry) should have been evicted.
	_, err = r.Resolve(context.Background(), firstURI)
	require.NoError(t, err)
	assert.Equal(t, int32(maxCredentialCacheEntries+2), repo.callCount.Load(),
		"first URI should miss cache after it was evicted as oldest-by-expiry")
}

// Verifies batch eviction removes ~10% entries to provide headroom for burst
// traffic, not just a single entry.
func TestCredentialResolver_BatchEvictionProvidesHeadroom(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	// Fill cache to exactly maxCredentialCacheEntries.
	for i := 0; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://entry-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	require.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// Trigger one overflow to invoke batch eviction.
	_, err := r.Resolve(context.Background(), "file://trigger-batch")
	require.NoError(t, err)

	stats := r.CacheStats()
	evicted := maxCredentialCacheEntries + 1 - stats.Size
	tenPercent := maxCredentialCacheEntries / 10

	assert.GreaterOrEqual(t, evicted, tenPercent,
		"batch eviction should remove at least 10%% of max entries (%d), but only removed %d",
		tenPercent, evicted)

	// Verify the earliest entries were the ones evicted (they have oldest expiry).
	// Entries 0..tenPercent-1 should be gone; re-resolving them should cause backend calls.
	callsBefore := repo.callCount.Load()
	for i := 0; i < tenPercent; i++ {
		uri := fmt.Sprintf("file://entry-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	callsAfter := repo.callCount.Load()
	refetched := callsAfter - callsBefore
	assert.Equal(t, int32(tenPercent), refetched,
		"all %d oldest entries should have been evicted and require re-fetch", tenPercent)
}

// TestCredentialResolver_CachedCredentials_ReturnIndependentCopies verifies that
// consecutive Resolve calls return independent deep copies so callers cannot
// corrupt cached state by mutating the returned CredentialSet.
func TestCredentialResolver_CachedCredentials_ReturnSharedPointer(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "shared", Password: "secret"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	got1, err := r.Resolve(context.Background(), "file://shared-ptr")
	require.NoError(t, err)

	got2, err := r.Resolve(context.Background(), "file://shared-ptr")
	require.NoError(t, err)

	assert.NotSame(t, got1, got2, "cached Resolve should return independent copies")
	assert.Equal(t, int32(1), repo.callCount.Load(), "repo should only be called once")
}

// Verifies resolveRepo returns an error for a malformed URI.
func TestCredentialResolver_ResolveRepo_InvalidURI(t *testing.T) {
	r := NewCredentialResolver()

	_, err := r.Resolve(context.Background(), "://missing-scheme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid URI")
}

// Verifies CacheStats accurately reports expired entries.
func TestCredentialResolver_CacheStats_AccurateExpiredCount(t *testing.T) {
	creds := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "stats-user"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	const ttl = 10 * time.Millisecond
	clk := clocktest.New()
	r := NewCredentialResolver(WithCredentialCacheTTL(ttl), WithCredentialClock(clk))
	r.Register(repo)

	uris := []string{"file://a", "file://b", "file://c"}
	for _, uri := range uris {
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}

	stats := r.CacheStats()
	assert.Equal(t, 3, stats.Size)
	assert.Equal(t, 3, stats.Active)
	assert.Equal(t, 0, stats.Expired)

	clk.Advance(ttl + time.Nanosecond)

	stats = r.CacheStats()
	assert.Equal(t, 3, stats.Size, "expired entries remain in cache until evicted")
	assert.Equal(t, 3, stats.Expired)
	assert.Equal(t, 0, stats.Active)
}

// TestCredentialResolver_Register_MultipleRepos_SameSchemeNamespace verifies
// which repository wins when two repos share the same scheme and namespace.
// The first registered repo wins because resolveRepo uses strict > comparison,
// so equal-length namespaces do not replace an existing best match.
func TestCredentialResolver_Register_MultipleRepos_SameSchemeNamespace(t *testing.T) {
	credsA := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "repoA"},
	}
	credsB := &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "repoB"},
	}
	repoA := &stubRepo{scheme: "pms", namespace: "tenant", creds: credsA}
	repoB := &stubRepo{scheme: "pms", namespace: "tenant", creds: credsB}

	r := NewCredentialResolver()
	r.Register(repoA)
	r.Register(repoB)

	got, err := r.Resolve(context.Background(), "pms://tenant/secret")
	require.NoError(t, err)

	// resolveRepo uses `len(ns) > bestLen` (strict greater-than), so the first
	// registered repo with equal namespace length wins.
	assert.Equal(t, "repoA", got.Password.Username,
		"first registered repo should win when namespace lengths are equal")
	assert.Equal(t, int32(1), repoA.callCount.Load())
	assert.Equal(t, int32(0), repoB.callCount.Load())
}

func TestNewCredentialResolver_NilClockOptionKeepsDefault(t *testing.T) {
	// Regression: WithCredentialClock(nil) must not clobber the clock.System
	// default, otherwise the first cache touch panics on r.clk.Now().
	r := NewCredentialResolver(WithCredentialClock(nil))

	creds := &connectivity.CredentialSet{Password: &connectivity.PasswordCredential{Username: "x"}}
	repo := &stubRepo{scheme: "pms", namespace: "tenant", creds: creds}
	r.Register(repo)

	require.NotPanics(t, func() {
		_, err := r.Resolve(context.Background(), "pms://tenant/secret")
		require.NoError(t, err)
	})
}
