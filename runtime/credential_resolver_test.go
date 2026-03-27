package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRepo struct {
	scheme    string
	namespace string
	creds     *domain.CredentialSet
	callCount atomic.Int32
}

func (s *stubRepo) Scheme() string    { return s.scheme }
func (s *stubRepo) Namespace() string { return s.namespace }
func (s *stubRepo) Get(_ context.Context, _ string) (*domain.CredentialSet, error) {
	s.callCount.Add(1)
	return s.creds, nil
}

// Verifies Resolve dispatches to the registered repository for the URI scheme and returns credentials.
func TestCredentialResolver_SingleSchemeDispatch(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
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
	fileCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "file-user", Password: "fp"},
	}
	pmsCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "pms-user", Password: "pp"},
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
	rootCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "root"},
	}
	tenantACreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "tenantA"},
	}
	app1Creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "app1"},
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
	assert.True(t, errors.Is(err, domain.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// Verifies a second Resolve for the same URI with TTL cache hits the store only once.
func TestCredentialResolver_CacheHitMiss(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "cached"},
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "expiring"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(10 * time.Millisecond))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(1), repo.callCount.Load())

	time.Sleep(20 * time.Millisecond)

	_, err = r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(2), repo.callCount.Load(), "expired cache entry should cause re-fetch")
}

// Verifies WithCredentialCacheDisabled causes every Resolve to call the underlying repository.
func TestCredentialResolver_CacheDisabled(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "nocache"},
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "inv"},
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "clear"},
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
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
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)
}

// Verifies when over capacity, expired entries are evicted before LRU-by-expiry eviction runs.
func TestCredentialResolver_CacheEvictsExpired(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Millisecond))
	r.Register(repo)

	for i := range maxCredentialCacheEntries {
		uri := fmt.Sprintf("file://old-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	time.Sleep(20 * time.Millisecond)

	_, err := r.Resolve(context.Background(), "file://fresh-after-expiry")
	require.NoError(t, err)

	stats := r.CacheStats()
	assert.Equal(t, 1, stats.Size, "expired bulk should be dropped when inserting past max size")
	assert.Equal(t, 0, stats.Expired, "remaining entry should be active")
	assert.Equal(t, int32(maxCredentialCacheEntries+1), repo.callCount.Load())
}

// Verifies when still over capacity after removing expired entries, the entry closest to expiry (earliest expiresAt) is evicted.
func TestCredentialResolver_CacheEvictsOldestWhenFull(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	firstURI := "file://first-evict-candidate"
	_, err := r.Resolve(context.Background(), firstURI)
	require.NoError(t, err)

	for i := 1; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://fill-%d", i)
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	overflowURI := "file://overflow"
	_, err = r.Resolve(context.Background(), overflowURI)
	require.NoError(t, err)
	assert.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	_, err = r.Resolve(context.Background(), firstURI)
	require.NoError(t, err)
	assert.Equal(t, int32(maxCredentialCacheEntries+2), repo.callCount.Load(),
		"first URI should miss cache after it was evicted as oldest-by-expiry")
}
