package retry

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// MemoryRetryManager is an in-memory implementation of types.RetryManager.
type MemoryRetryManager struct {
	policy  types.RetryPolicy
	dlq     types.DeadLetterQueue
	pending *retryQueue
	mu      sync.Mutex
	stats   types.RetryStats
	running atomic.Bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewMemoryRetryManager creates a new in-memory retry manager.
func NewMemoryRetryManager(policy types.RetryPolicy, dlq types.DeadLetterQueue) *MemoryRetryManager {
	if dlq == nil {
		dlq = NewMemoryDLQ()
	}

	return &MemoryRetryManager{
		policy:  policy,
		dlq:     dlq,
		pending: newRetryQueue(),
		stopCh:  make(chan struct{}),
	}
}

// Enqueue adds a message to the retry queue.
func (m *MemoryRetryManager) Enqueue(ctx context.Context, msg types.Message, reason error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get or create retry info
	info := m.getRetryInfo(&msg)
	info.Attempt++
	info.LastAttemptAt = time.Now()
	info.LastError = reason.Error()

	// Check if exhausted
	if info.IsExhausted() {
		atomic.AddInt64(&m.stats.Failed, 1)
		return m.dlq.Send(ctx, msg, reason)
	}

	// Calculate next retry time
	info.NextRetryAt = nextRetryTime(m.policy, info.Attempt)

	// Store retry info in message metadata
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata["_retryInfo"] = info

	// Add to queue
	item := &retryItem{
		message:     msg,
		nextRetryAt: info.NextRetryAt,
		attempt:     info.Attempt,
	}
	heap.Push(m.pending, item)

	atomic.AddInt64(&m.stats.Pending, 1)
	atomic.AddInt64(&m.stats.TotalAttempts, 1)

	return nil
}

// Start begins processing retries.
func (m *MemoryRetryManager) Start(ctx context.Context, handler types.Subscriber) error {
	if !m.running.CompareAndSwap(false, true) {
		return nil // Already running
	}

	m.wg.Add(1)
	go m.processLoop(ctx, handler)

	return nil
}

// processLoop continuously processes messages ready for retry.
func (m *MemoryRetryManager) processLoop(ctx context.Context, handler types.Subscriber) {
	defer m.wg.Done()
	defer m.running.Store(false)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.processReadyMessages(ctx, handler)
		}
	}
}

// processReadyMessages processes all messages that are ready for retry.
func (m *MemoryRetryManager) processReadyMessages(ctx context.Context, handler types.Subscriber) {
	now := time.Now()

	for {
		m.mu.Lock()
		if m.pending.Len() == 0 {
			m.mu.Unlock()
			return
		}

		// Peek at the next item
		item := (*m.pending)[0]
		if item.nextRetryAt.After(now) {
			m.mu.Unlock()
			return
		}

		// Pop the item
		heap.Pop(m.pending)
		atomic.AddInt64(&m.stats.Pending, -1)
		atomic.AddInt64(&m.stats.InFlight, 1)
		m.mu.Unlock()

		// Process the message
		err := handler.Process(ctx, item.message.Topic, item.message)

		atomic.AddInt64(&m.stats.InFlight, -1)

		if err != nil {
			// Re-enqueue for another retry
			if enqErr := m.Enqueue(ctx, item.message, err); enqErr != nil {
				// Failed to re-enqueue - this shouldn't happen
				continue
			}
		} else {
			atomic.AddInt64(&m.stats.Succeeded, 1)
		}
	}
}

// Stop stops the retry manager.
func (m *MemoryRetryManager) Stop() {
	if m.running.Load() {
		close(m.stopCh)
		m.wg.Wait()
	}
}

// Stats returns current retry statistics.
func (m *MemoryRetryManager) Stats() types.RetryStats {
	return types.RetryStats{
		Pending:       atomic.LoadInt64(&m.stats.Pending),
		InFlight:      atomic.LoadInt64(&m.stats.InFlight),
		Succeeded:     atomic.LoadInt64(&m.stats.Succeeded),
		Failed:        atomic.LoadInt64(&m.stats.Failed),
		TotalAttempts: atomic.LoadInt64(&m.stats.TotalAttempts),
	}
}

// Purge removes all messages from the retry queue.
func (m *MemoryRetryManager) Purge(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pending = newRetryQueue()
	atomic.StoreInt64(&m.stats.Pending, 0)
	return nil
}

// DLQ returns the dead letter queue.
func (m *MemoryRetryManager) DLQ() types.DeadLetterQueue {
	return m.dlq
}

// getRetryInfo extracts or creates retry info for a message.
func (m *MemoryRetryManager) getRetryInfo(msg *types.Message) *types.RetryInfo {
	if msg.Metadata != nil {
		if info, ok := msg.Metadata["_retryInfo"].(*types.RetryInfo); ok {
			return info
		}
	}

	return &types.RetryInfo{
		Attempt:        0,
		MaxAttempts:    m.policy.MaxAttempts,
		FirstAttemptAt: time.Now(),
	}
}

// Ensure MemoryRetryManager implements types.RetryManager
var _ types.RetryManager = (*MemoryRetryManager)(nil)

// retryItem represents a message in the retry queue.
type retryItem struct {
	message     types.Message
	nextRetryAt time.Time
	attempt     int
	index       int // Index in the heap
}

// retryQueue is a priority queue of retry items, ordered by nextRetryAt.
type retryQueue []*retryItem

func newRetryQueue() *retryQueue {
	rq := make(retryQueue, 0)
	heap.Init(&rq)
	return &rq
}

func (rq retryQueue) Len() int { return len(rq) }

func (rq retryQueue) Less(i, j int) bool {
	return rq[i].nextRetryAt.Before(rq[j].nextRetryAt)
}

func (rq retryQueue) Swap(i, j int) {
	rq[i], rq[j] = rq[j], rq[i]
	rq[i].index = i
	rq[j].index = j
}

func (rq *retryQueue) Push(x any) {
	n := len(*rq)
	item := x.(*retryItem)
	item.index = n
	*rq = append(*rq, item)
}

func (rq *retryQueue) Pop() any {
	old := *rq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*rq = old[0 : n-1]
	return item
}
