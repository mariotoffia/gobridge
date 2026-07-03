package transport

import (
	"container/list"
	"sync"
)

// dedupWindow is a bounded, concurrency-safe LRU set of recently
// processed ingress idempotency keys (Idempotency-Key / X-Dedup-Id).
//
// Semantics (see doc.go "Ingress idempotency window"):
//
//   - A key is recorded ONLY after the delivery it belongs to was
//     processed successfully (acked). A failed attempt never records,
//     so a legitimate client retry after a 5xx is re-processed.
//   - The window is node-local and best-effort: it bounds duplicate
//     deliveries from forward retries and client retries within the
//     remembered window, it does not make ingress exactly-once. Two
//     concurrent requests with the same key may both process (the
//     check-then-record is not transactional by design).
//   - Capacity-bounded: the least-recently-seen key is evicted when
//     the window is full, so memory stays O(capacity).
type dedupWindow struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recently seen
	keys     map[string]*list.Element
}

func newDedupWindow(capacity int) *dedupWindow {
	if capacity <= 0 {
		capacity = defaultDedupWindow
	}
	return &dedupWindow{
		capacity: capacity,
		order:    list.New(),
		keys:     make(map[string]*list.Element, capacity),
	}
}

// seen reports whether key is inside the window, refreshing its
// recency when present.
func (d *dedupWindow) seen(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.keys[key]
	if ok {
		d.order.MoveToFront(el)
	}
	return ok
}

// record inserts key as most-recently seen, evicting the oldest entry
// when the window is at capacity. Empty keys are ignored.
func (d *dedupWindow) record(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.keys[key]; ok {
		d.order.MoveToFront(el)
		return
	}
	if d.order.Len() >= d.capacity {
		oldest := d.order.Back()
		if oldest != nil {
			d.order.Remove(oldest)
			delete(d.keys, oldest.Value.(string))
		}
	}
	d.keys[key] = d.order.PushFront(key)
}
