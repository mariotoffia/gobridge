package retry

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// MemoryDLQ is an in-memory implementation of types.DeadLetterQueue.
type MemoryDLQ struct {
	messages []*types.DLQMessage
	mu       sync.RWMutex
}

// NewMemoryDLQ creates a new in-memory dead letter queue.
func NewMemoryDLQ() *MemoryDLQ {
	return &MemoryDLQ{
		messages: make([]*types.DLQMessage, 0),
	}
}

// Send moves a message to the dead letter queue.
func (d *MemoryDLQ) Send(ctx context.Context, msg types.Message, reason error) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	dlqMsg := &types.DLQMessage{
		Message:  msg,
		Reason:   reason.Error(),
		FailedAt: time.Now(),
	}

	// Extract retry info from metadata if present
	if msg.Metadata != nil {
		if info, ok := msg.Metadata["_retryInfo"].(*types.RetryInfo); ok {
			dlqMsg.RetryInfo = info
		}
	}

	d.messages = append(d.messages, dlqMsg)
	return nil
}

// Consume returns a channel of messages from the DLQ.
func (d *MemoryDLQ) Consume(ctx context.Context) (<-chan *types.DLQMessage, error) {
	ch := make(chan *types.DLQMessage)

	go func() {
		defer close(ch)

		d.mu.RLock()
		msgs := make([]*types.DLQMessage, len(d.messages))
		copy(msgs, d.messages)
		d.mu.RUnlock()

		for _, msg := range msgs {
			select {
			case <-ctx.Done():
				return
			case ch <- msg:
			}
		}
	}()

	return ch, nil
}

// Count returns the number of messages in the DLQ.
func (d *MemoryDLQ) Count(ctx context.Context) (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int64(len(d.messages)), nil
}

// Purge removes all messages from the DLQ.
func (d *MemoryDLQ) Purge(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = make([]*types.DLQMessage, 0)
	return nil
}

// Replay moves messages from DLQ back to the retry queue.
func (d *MemoryDLQ) Replay(ctx context.Context, filter types.DLQFilter) (replayed int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var remaining []*types.DLQMessage
	var count int64

	for _, msg := range d.messages {
		if d.matchesFilter(msg, filter) {
			if filter.MaxMessages > 0 && count >= filter.MaxMessages {
				remaining = append(remaining, msg)
				continue
			}
			// Message will be replayed (caller handles actual replay)
			count++
		} else {
			remaining = append(remaining, msg)
		}
	}

	d.messages = remaining
	return count, nil
}

// matchesFilter checks if a DLQ message matches the filter criteria.
func (d *MemoryDLQ) matchesFilter(msg *types.DLQMessage, filter types.DLQFilter) bool {
	if filter.Topic != "" && msg.Message.Topic != filter.Topic {
		return false
	}
	if !filter.Since.IsZero() && msg.FailedAt.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && msg.FailedAt.After(filter.Until) {
		return false
	}
	if filter.SourceID != "" && msg.SourceID != filter.SourceID {
		return false
	}
	return true
}

// GetMessages returns all messages in the DLQ (for testing).
func (d *MemoryDLQ) GetMessages() []*types.DLQMessage {
	d.mu.RLock()
	defer d.mu.RUnlock()
	msgs := make([]*types.DLQMessage, len(d.messages))
	copy(msgs, d.messages)
	return msgs
}

// Ensure MemoryDLQ implements types.DeadLetterQueue
var _ types.DeadLetterQueue = (*MemoryDLQ)(nil)
