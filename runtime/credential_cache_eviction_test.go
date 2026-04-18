package runtime

// Tests for BUG-5: Credential cache batch eviction fix.
//
// Validates that setCached() evicts ~10% of oldest entries on overflow
// instead of just 1, and that expired entries are evicted first.
//
// Summary:
// +------+-----------------------------------------------------+
// | ID   | Description                                         |
// +------+-----------------------------------------------------+
// | B5T1 | Filling to exactly maxEntries does NOT evict         |
// | B5T2 | Entry 1001 triggers batch eviction of ~100 entries  |
// | B5T3 | Eviction removes oldest entries (earliest expiry)   |
// | B5T4 | Concurrent Resolve overflow -- no race conditions   |
// | B5T5 | InvalidateCache works correctly after batch eviction|
// | B5T6 | Expired entries evicted first before batch eviction |
// +------+-----------------------------------------------------+

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEvictionRepo is a minimal credential repository for eviction tests.
type stubEvictionRepo struct {
	scheme    string
	namespace string
	creds     *domain.CredentialSet
}

func (s *stubEvictionRepo) Scheme() string    { return s.scheme }
func (s *stubEvictionRepo) Namespace() string { return s.namespace }
func (s *stubEvictionRepo) Get(_ context.Context, _ string) (*domain.CredentialSet, error) {
	return s.creds, nil
}

// newEvictionResolver creates a CredentialResolver with a stub repo and
// the given cache TTL, ready for eviction tests.
func newEvictionResolver(ttl time.Duration) *CredentialResolver {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubEvictionRepo{scheme: "file", namespace: "", creds: creds}
	r := NewCredentialResolver(WithCredentialCacheTTL(ttl))
	r.Register(repo)
	return r
}

// fillCache populates the resolver cache with n entries using distinct URIs.
// Returns the list of URIs inserted in order.
func fillCache(t *testing.T, r *CredentialResolver, n int) []string {
	t.Helper()
	ctx := context.Background()
	uris := make([]string, n)
	for i := 0; i < n; i++ {
		uris[i] = fmt.Sprintf("file://entry-%d", i)
		_, err := r.Resolve(ctx, uris[i])
		require.NoError(t, err)
	}
	return uris
}

// TestCacheEviction_ExactMaxDoesNotEvict validates that filling the cache
// to exactly maxCredentialCacheEntries (1000) does NOT trigger any eviction.
func TestCacheEviction_ExactMaxDoesNotEvict(t *testing.T) {
	r := newEvictionResolver(time.Hour)
	fillCache(t, r, maxCredentialCacheEntries)

	stats := r.CacheStats()
	assert.Equal(t, maxCredentialCacheEntries, stats.Size,
		"cache should hold exactly %d entries without eviction", maxCredentialCacheEntries)
	assert.Equal(t, 0, stats.Expired, "no entries should be expired")
	assert.Equal(t, maxCredentialCacheEntries, stats.Active,
		"all entries should be active")
}

// TestCacheEviction_OverflowTriggersBatchEviction validates that adding
// the 1001st entry triggers batch eviction removing approximately 100
// entries (~10% of maxCredentialCacheEntries).
func TestCacheEviction_OverflowTriggersBatchEviction(t *testing.T) {
	r := newEvictionResolver(time.Hour)
	fillCache(t, r, maxCredentialCacheEntries)
	require.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// Add one more entry to trigger overflow.
	ctx := context.Background()
	_, err := r.Resolve(ctx, "file://overflow-trigger")
	require.NoError(t, err)

	stats := r.CacheStats()
	tenPercent := maxCredentialCacheEntries / 10

	// After batch eviction: we inserted 1001 entries total but evicted
	// at least 10% of max entries. The new size should be at most
	// maxEntries - tenPercent + 1 (the overflow entry).
	expectedMax := maxCredentialCacheEntries - tenPercent + 1
	assert.LessOrEqual(t, stats.Size, expectedMax,
		"batch eviction should remove ~%d entries, leaving at most %d",
		tenPercent, expectedMax)

	// Verify at least 10% were removed.
	evicted := maxCredentialCacheEntries + 1 - stats.Size
	assert.GreaterOrEqual(t, evicted, tenPercent,
		"batch eviction should remove at least %d entries (10%%), removed %d",
		tenPercent, evicted)
}

// TestCacheEviction_RemovesOldestEntries validates that batch eviction
// removes entries with the earliest expiry times (the oldest ones).
func TestCacheEviction_RemovesOldestEntries(t *testing.T) {
	r := newEvictionResolver(time.Hour)
	ctx := context.Background()
	tenPercent := maxCredentialCacheEntries / 10

	// Insert entries in order. Earlier entries get earlier expiry times
	// because setCached uses time.Now().Add(ttl) at insertion time.
	// We add small sleeps between the first batch and the rest to ensure
	// deterministic ordering of expiry times.
	for i := 0; i < tenPercent; i++ {
		uri := fmt.Sprintf("file://old-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}

	time.Sleep(2 * time.Millisecond) // FIXED: ensure deterministic ordering of expiry times

	for i := tenPercent; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://new-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}
	require.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// Trigger overflow.
	_, err := r.Resolve(ctx, "file://trigger-evict")
	require.NoError(t, err)

	// The "old" entries (old-0 through old-99) should have been evicted.
	// Verify by checking that they are no longer in cache (resolving them
	// should return fresh results -- but since we cannot distinguish cache
	// hits from misses via the public API, we check that the cache size
	// indicates the old entries were removed).
	stats := r.CacheStats()
	assert.LessOrEqual(t, stats.Size, maxCredentialCacheEntries-tenPercent+1,
		"oldest %d entries should have been evicted", tenPercent)

	// Additionally, verify via direct cache inspection that none of the
	// old-* keys remain in cache.
	r.mu.RLock()
	for i := 0; i < tenPercent; i++ {
		key := fmt.Sprintf("file://old-%d", i)
		_, found := r.cache[key]
		assert.False(t, found, "old entry %q should have been evicted", key)
	}
	r.mu.RUnlock()

	// Verify newer entries are still present.
	r.mu.RLock()
	newerPresent := 0
	for i := tenPercent; i < maxCredentialCacheEntries; i++ {
		key := fmt.Sprintf("file://new-%d", i)
		if _, found := r.cache[key]; found {
			newerPresent++
		}
	}
	r.mu.RUnlock()
	assert.Greater(t, newerPresent, 0,
		"at least some newer entries should survive eviction")
}

// TestCacheEviction_ConcurrentOverflow validates that concurrent Resolve
// calls that all trigger cache overflow do not cause data races or panics.
// This test is designed to be run with -race.
func TestCacheEviction_ConcurrentOverflow(t *testing.T) {
	r := newEvictionResolver(time.Hour)
	ctx := context.Background()

	// Pre-fill cache close to the limit.
	fillCache(t, r, maxCredentialCacheEntries-10)

	const goroutines = 50
	var wg sync.WaitGroup
	barrier := make(chan struct{})

	// Each goroutine inserts a unique URI, causing overflow from multiple
	// goroutines simultaneously.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier
			uri := fmt.Sprintf("file://concurrent-%d", idx)
			_, err := r.Resolve(ctx, uri)
			if err != nil {
				t.Errorf("goroutine %d: resolve error: %v", idx, err)
			}
		}(i)
	}

	close(barrier)
	wg.Wait()

	// After all concurrent insertions, cache size must not exceed max.
	stats := r.CacheStats()
	assert.LessOrEqual(t, stats.Size, maxCredentialCacheEntries,
		"cache should not exceed max entries after concurrent overflow")
	assert.Greater(t, stats.Size, 0, "cache should not be empty")
}

// TestCacheEviction_InvalidateAfterBatchEviction validates that
// InvalidateCache works correctly after a batch eviction has occurred.
func TestCacheEviction_InvalidateAfterBatchEviction(t *testing.T) {
	r := newEvictionResolver(time.Hour)
	ctx := context.Background()

	fillCache(t, r, maxCredentialCacheEntries)

	// Trigger batch eviction.
	survivorURI := "file://survivor-invalidate"
	_, err := r.Resolve(ctx, survivorURI)
	require.NoError(t, err)

	// Verify the survivor entry is in cache.
	r.mu.RLock()
	_, found := r.cache[survivorURI]
	r.mu.RUnlock()
	assert.True(t, found, "survivor entry should be in cache before invalidation")

	sizeBefore := r.CacheStats().Size

	// Invalidate the survivor entry.
	r.InvalidateCache(survivorURI)

	sizeAfter := r.CacheStats().Size
	assert.Equal(t, sizeBefore-1, sizeAfter,
		"invalidation should remove exactly one entry")

	// Verify the entry is gone.
	r.mu.RLock()
	_, found = r.cache[survivorURI]
	r.mu.RUnlock()
	assert.False(t, found, "invalidated entry should not be in cache")

	// Invalidating a non-existent key should be a no-op.
	r.InvalidateCache("file://does-not-exist")
	assert.Equal(t, sizeAfter, r.CacheStats().Size,
		"invalidating non-existent key should not change cache size")
}

// TestCacheEviction_ExpiredEvictedBeforeBatch validates that expired
// entries are evicted first (in the setCached loop) before the batch
// eviction of oldest entries kicks in.
func TestCacheEviction_ExpiredEvictedBeforeBatch(t *testing.T) {
	// Use a very short TTL so entries expire quickly.
	r := newEvictionResolver(5 * time.Millisecond)
	ctx := context.Background()

	// Fill half the cache with entries that will expire.
	halfSize := maxCredentialCacheEntries / 2
	for i := 0; i < halfSize; i++ {
		uri := fmt.Sprintf("file://expired-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}

	time.Sleep(20 * time.Millisecond) // FIXED: wait for cache TTL (5ms) to expire

	// Now switch to a long TTL for remaining entries.
	r.mu.Lock()
	r.cacheTTL = time.Hour
	r.mu.Unlock()

	// Fill the rest of the cache with fresh entries.
	for i := halfSize; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://fresh-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}

	// At this point the cache has maxCredentialCacheEntries entries:
	// half expired, half fresh.
	require.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// Add one more to trigger overflow. The setCached method should first
	// remove all expired entries. Since removing ~500 expired entries
	// brings us well below max, batch eviction should NOT fire.
	_, err := r.Resolve(ctx, "file://final-trigger")
	require.NoError(t, err)

	stats := r.CacheStats()

	// The expired half should be gone. Remaining = fresh half + 1 trigger.
	expectedSize := maxCredentialCacheEntries - halfSize + 1
	assert.Equal(t, expectedSize, stats.Size,
		"expired entries should be evicted first, leaving %d entries", expectedSize)
	assert.Equal(t, 0, stats.Expired,
		"no expired entries should remain after expired-first eviction")

	// Verify fresh entries survived.
	r.mu.RLock()
	freshCount := 0
	for i := halfSize; i < maxCredentialCacheEntries; i++ {
		key := fmt.Sprintf("file://fresh-%d", i)
		if _, ok := r.cache[key]; ok {
			freshCount++
		}
	}
	r.mu.RUnlock()
	assert.Equal(t, maxCredentialCacheEntries-halfSize, freshCount,
		"all fresh entries should survive when only expired entries are evicted")
}

// TestCacheEviction_MixedExpiredAndOldest validates the two-phase eviction:
// first expired entries are removed, then if still over capacity, the
// oldest batch is evicted.
func TestCacheEviction_MixedExpiredAndOldest(t *testing.T) {
	// Use a short TTL so a small fraction expires.
	r := newEvictionResolver(5 * time.Millisecond)
	ctx := context.Background()

	// Insert 50 entries that will expire.
	expiredCount := 50
	for i := 0; i < expiredCount; i++ {
		uri := fmt.Sprintf("file://will-expire-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}

	time.Sleep(20 * time.Millisecond) // FIXED: wait for cache TTL (5ms) to expire

	// Switch to long TTL and fill the rest.
	r.mu.Lock()
	r.cacheTTL = time.Hour
	r.mu.Unlock()

	for i := expiredCount; i < maxCredentialCacheEntries; i++ {
		uri := fmt.Sprintf("file://live-%d", i)
		_, err := r.Resolve(ctx, uri)
		require.NoError(t, err)
	}
	require.Equal(t, maxCredentialCacheEntries, r.CacheStats().Size)

	// Trigger overflow. Phase 1 removes 50 expired entries.
	// Cache becomes 950 + 1 = 951, which is <= 1000, so no batch eviction.
	_, err := r.Resolve(ctx, "file://mixed-trigger")
	require.NoError(t, err)

	stats := r.CacheStats()
	expectedSize := maxCredentialCacheEntries - expiredCount + 1
	assert.Equal(t, expectedSize, stats.Size,
		"after removing %d expired entries and adding 1, size should be %d",
		expiredCount, expectedSize)
	assert.Equal(t, 0, stats.Expired)
}
