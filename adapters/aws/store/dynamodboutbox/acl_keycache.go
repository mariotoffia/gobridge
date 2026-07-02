package dynamodboutbox

import (
	"container/list"
	"sync"
)

// defaultMaxKeyCache bounds the record-ID→base-key LRU (see keyCache). It
// comfortably exceeds the realistic in-flight working set (a process's
// simultaneously claimed-but-not-completed outbox records across every owned
// partition) so the cap never evicts a live record on healthy traffic, while
// capping worst-case memory to a few MB under lease churn.
const defaultMaxKeyCache = 100_000

// keyCache is a bounded, least-recently-used map from an application record
// ID to its base-table (PK, SK). It exists purely so Complete can address a
// record directly instead of resolving through the eventually consistent
// RecordIDIndex GSI (which can lag and report not-found, reopening the
// duplicate-delivery window J2/J3 close). Because a miss merely falls back to
// that bounded GSI resolve, evicting any entry is always correctness-safe.
//
// Entries are added on Claim and removed on terminal Complete. Records this
// instance claimed but never completes — e.g. after a lease transfer hands
// the partition to another owner — are never removed by Complete, so under
// lease churn a plain map grew without bound (J-N1). The LRU cap fixes that:
// it evicts the OLDEST (dead) entries first and retains the hot,
// recently-claimed keys a Complete is about to need, so bounding the cache
// does not reopen the GSI-lag duplicate window on live records.
type keyCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List // front = most-recently-used
	items map[string]*list.Element
}

type keyCacheEntry struct {
	id  string
	key recordKey
}

func newKeyCache(max int) *keyCache {
	if max <= 0 {
		max = defaultMaxKeyCache
	}
	return &keyCache{
		max:   max,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}
}

// put inserts or refreshes id→key, marking it most-recently-used and evicting
// the least-recently-used entry when the cache is over capacity.
func (c *keyCache) put(id string, key recordKey) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[id]; ok {
		el.Value.(*keyCacheEntry).key = key
		c.ll.MoveToFront(el)
		return
	}

	c.items[id] = c.ll.PushFront(&keyCacheEntry{id: id, key: key})
	if c.ll.Len() > c.max {
		if oldest := c.ll.Back(); oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*keyCacheEntry).id)
		}
	}
}

// get returns the base key for id, refreshing its recency on a hit.
func (c *keyCache) get(id string) (recordKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[id]
	if !ok {
		return recordKey{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*keyCacheEntry).key, true
}

// remove drops id from the cache (a record becoming terminal on Complete).
func (c *keyCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[id]; ok {
		c.ll.Remove(el)
		delete(c.items, id)
	}
}

// len reports the number of cached entries (bound assertion in tests).
func (c *keyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
