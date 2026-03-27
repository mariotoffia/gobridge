package runtime

import (
	"sync"
	"time"
)

type depthEntry struct {
	atCapacity bool
	checkedAt  time.Time
}

// outboxDepthCache caches the result of outbox depth queries to avoid
// hitting the store on every ingress message. Entries expire after the
// configured TTL, at which point the next call triggers a fresh query.
type outboxDepthCache struct {
	mu      sync.RWMutex
	entries map[string]depthEntry
	ttl     time.Duration
}

func newOutboxDepthCache(ttl time.Duration) *outboxDepthCache {
	return &outboxDepthCache{
		entries: make(map[string]depthEntry),
		ttl:     ttl,
	}
}

// isUnderCapacity returns true if the cache has a fresh "not at capacity"
// entry for the given partition key. Returns false if the entry is absent,
// expired, or indicates the outbox is at capacity (forcing a real query).
func (c *outboxDepthCache) isUnderCapacity(partitionKey string) bool {
	c.mu.RLock()
	entry, ok := c.entries[partitionKey]
	c.mu.RUnlock()

	if !ok {
		return false
	}
	if time.Since(entry.checkedAt) > c.ttl {
		return false
	}
	return !entry.atCapacity
}

const depthCacheMaxEntries = 1000

func (c *outboxDepthCache) update(partitionKey string, atCapacity bool) {
	now := time.Now()
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
		if len(c.entries) > depthCacheMaxEntries {
			c.entries = map[string]depthEntry{
				partitionKey: {atCapacity: atCapacity, checkedAt: now},
			}
		}
	}
	c.mu.Unlock()
}
