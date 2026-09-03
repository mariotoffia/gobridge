package outbox

import (
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

type depthEntry struct {
	atCapacity bool
	checkedAt  time.Time
}

// DepthCache caches the result of outbox depth queries to avoid hitting
// the store on every ingress message. Entries expire after the
// configured TTL, at which point the next call triggers a fresh query.
type DepthCache struct {
	mu      sync.RWMutex
	entries map[string]depthEntry
	ttl     time.Duration
	clk     clock.Clock
}

// NewDepthCache constructs a DepthCache with the given TTL and clock.
func NewDepthCache(ttl time.Duration, clk clock.Clock) *DepthCache {
	return &DepthCache{
		entries: make(map[string]depthEntry),
		ttl:     ttl,
		clk:     clk,
	}
}

// IsUnderCapacity returns true if the cache has a fresh "not at capacity"
// entry for the given partition key. Returns false if the entry is absent,
// expired, or indicates the outbox is at capacity (forcing a real query).
func (c *DepthCache) IsUnderCapacity(partitionKey string) bool {
	c.mu.RLock()
	entry, ok := c.entries[partitionKey]
	c.mu.RUnlock()

	if !ok {
		return false
	}
	if c.clk.Now().Sub(entry.checkedAt) > c.ttl {
		return false
	}
	return !entry.atCapacity
}

const depthCacheMaxEntries = 1000

// Update records the capacity status for the given partition key with
// the current timestamp, evicting stale entries if the cache grows
// beyond depthCacheMaxEntries.
func (c *DepthCache) Update(partitionKey string, atCapacity bool) {
	now := c.clk.Now()
	c.mu.Lock()
	c.entries[partitionKey] = depthEntry{
		atCapacity: atCapacity,
		checkedAt:  now,
	}
	if len(c.entries) > depthCacheMaxEntries {
		cutoff := now.Add(-c.ttl * 10)
		for k, e := range c.entries {
			if e.checkedAt.Before(cutoff) {
				delete(c.entries, k)
			}
		}
		// If the stale sweep did not free enough, evict entries
		// ONE AT A TIME (random order — Go map iteration is randomized) until
		// back within the bound, never below it. The previous code collapsed
		// the entire cache to a single entry, which dropped every other
		// partition's fresh "under capacity" verdict and triggered a
		// depth-query stampede against the store on the next ingress burst.
		// Never evict the just-written key so this Update is not immediately
		// undone.
		for k := range c.entries {
			if len(c.entries) <= depthCacheMaxEntries {
				break
			}
			if k == partitionKey {
				continue
			}
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}
