package retry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// DLQManager provides a high-level API for managing dead letter queues.
type DLQManager struct {
	mu sync.RWMutex

	// queues maps pipeline IDs to their DLQs
	queues map[string]types.DeadLetterQueue

	// messages is an in-memory index of all messages (for the default implementation)
	messages map[string]*indexedMessage

	// replayHandler is called when messages are replayed
	replayHandler ReplayHandler
}

// indexedMessage wraps a DLQ message with additional metadata.
type indexedMessage struct {
	*types.DLQMessageWithID
	PipelineID string
	IndexedAt  time.Time
}

// ReplayHandler handles replayed messages.
type ReplayHandler func(ctx context.Context, pipelineID string, msg *types.Message) error

// DLQManagerOption configures a DLQManager.
type DLQManagerOption func(*DLQManager)

// WithReplayHandler sets the replay handler.
func WithReplayHandler(handler ReplayHandler) DLQManagerOption {
	return func(m *DLQManager) {
		m.replayHandler = handler
	}
}

// NewDLQManager creates a new DLQ manager.
func NewDLQManager(opts ...DLQManagerOption) *DLQManager {
	m := &DLQManager{
		queues:   make(map[string]types.DeadLetterQueue),
		messages: make(map[string]*indexedMessage),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// RegisterQueue registers a DLQ for a pipeline.
func (m *DLQManager) RegisterQueue(pipelineID string, queue types.DeadLetterQueue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[pipelineID] = queue
}

// GetQueue returns the DLQ for a pipeline.
func (m *DLQManager) GetQueue(pipelineID string) (types.DeadLetterQueue, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q, ok := m.queues[pipelineID]
	return q, ok
}

// SendToDLQ sends a message to the appropriate DLQ.
func (m *DLQManager) SendToDLQ(ctx context.Context, pipelineID string, msg types.Message, reason error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	queue, ok := m.queues[pipelineID]
	if !ok {
		// Create a default queue if none exists
		queue = NewMemoryDLQ()
		m.queues[pipelineID] = queue
	}

	// Send to the underlying queue
	if err := queue.Send(ctx, msg, reason); err != nil {
		return err
	}

	// Index the message for management
	id := generateMessageID()
	indexed := &indexedMessage{
		DLQMessageWithID: &types.DLQMessageWithID{
			DLQMessage: types.DLQMessage{
				Message:  msg,
				Reason:   reason.Error(),
				FailedAt: time.Now(),
				SourceID: pipelineID,
			},
			ID:         id,
			PipelineID: pipelineID,
			ErrorCode:  extractErrorCode(reason),
		},
		PipelineID: pipelineID,
		IndexedAt:  time.Now(),
	}

	// Create payload preview
	if len(msg.Payload) > 500 {
		indexed.PayloadPreview = string(msg.Payload[:500]) + "..."
	} else {
		indexed.PayloadPreview = string(msg.Payload)
	}

	m.messages[id] = indexed

	return nil
}

// GetSummary returns a summary of all DLQ statistics.
func (m *DLQManager) GetSummary(ctx context.Context) (*types.DLQSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	summary := &types.DLQSummary{
		ByPipeline:  make(map[string]int64),
		ByTopic:     make(map[string]int64),
		ByErrorCode: make(map[string]int64),
	}

	var oldest, newest *time.Time

	for _, msg := range m.messages {
		summary.TotalMessages++
		summary.ByPipeline[msg.PipelineID]++
		summary.ByTopic[msg.Message.Topic]++
		if msg.ErrorCode != "" {
			summary.ByErrorCode[msg.ErrorCode]++
		}

		if oldest == nil || msg.FailedAt.Before(*oldest) {
			t := msg.FailedAt
			oldest = &t
		}
		if newest == nil || msg.FailedAt.After(*newest) {
			t := msg.FailedAt
			newest = &t
		}
	}

	summary.OldestMessage = oldest
	summary.NewestMessage = newest

	return summary, nil
}

// ListMessages returns messages from the DLQ with optional filtering.
func (m *DLQManager) ListMessages(ctx context.Context, filter types.DLQFilter, pagination types.DLQPagination) (*types.DLQMessageList, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if pagination.Limit <= 0 {
		pagination.Limit = 100
	}
	if pagination.Limit > 1000 {
		pagination.Limit = 1000
	}

	var filtered []*types.DLQMessage
	for _, msg := range m.messages {
		if matchesFilter(&msg.DLQMessage, filter) {
			filtered = append(filtered, &msg.DLQMessage)
		}
	}

	total := int64(len(filtered))

	// Apply pagination
	start := pagination.Offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pagination.Limit
	if end > len(filtered) {
		end = len(filtered)
	}

	result := filtered[start:end]

	return &types.DLQMessageList{
		Messages: result,
		Total:    total,
		Offset:   pagination.Offset,
		Limit:    pagination.Limit,
		HasMore:  end < len(filtered),
	}, nil
}

// GetMessage returns a specific message by ID.
func (m *DLQManager) GetMessage(ctx context.Context, messageID string) (*types.DLQMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	msg, ok := m.messages[messageID]
	if !ok {
		return nil, types.ErrNotFound
	}

	return &msg.DLQMessage, nil
}

// DeleteMessage removes a specific message from the DLQ.
func (m *DLQManager) DeleteMessage(ctx context.Context, messageID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.messages[messageID]; !ok {
		return types.ErrNotFound
	}

	delete(m.messages, messageID)
	return nil
}

// Replay moves messages from DLQ back to the retry queue.
func (m *DLQManager) Replay(ctx context.Context, request *types.DLQReplayRequest) (*types.DLQReplayResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.replayHandler == nil {
		return nil, fmt.Errorf("no replay handler configured")
	}

	result := &types.DLQReplayResult{}

	var toReplay []*indexedMessage

	if len(request.MessageIDs) > 0 {
		// Replay specific messages
		for _, id := range request.MessageIDs {
			if msg, ok := m.messages[id]; ok {
				toReplay = append(toReplay, msg)
			}
		}
	} else {
		// Replay based on filter
		for _, msg := range m.messages {
			if matchesFilter(&msg.DLQMessage, request.Filter) {
				toReplay = append(toReplay, msg)
			}
		}
	}

	for _, msg := range toReplay {
		targetPipeline := msg.PipelineID
		if request.TargetPipeline != "" {
			targetPipeline = request.TargetPipeline
		}

		// Optionally reset retry count
		replayMsg := msg.Message
		if request.ResetRetryCount && replayMsg.Metadata != nil {
			delete(replayMsg.Metadata, "_retryInfo")
			delete(replayMsg.Metadata, "_retryCount")
		}

		if err := m.replayHandler(ctx, targetPipeline, &replayMsg); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", msg.ID, err))
		} else {
			result.Replayed++
			result.ReplayedIDs = append(result.ReplayedIDs, msg.ID)
			delete(m.messages, msg.ID)
		}
	}

	return result, nil
}

// Purge removes messages from the DLQ based on filter criteria.
func (m *DLQManager) Purge(ctx context.Context, filter types.DLQFilter) (*types.DLQPurgeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := &types.DLQPurgeResult{}

	var toDelete []string
	for id, msg := range m.messages {
		if matchesFilter(&msg.DLQMessage, filter) {
			toDelete = append(toDelete, id)
		}
	}

	for _, id := range toDelete {
		delete(m.messages, id)
		result.Purged++
	}

	// Also purge from underlying queues
	for _, queue := range m.queues {
		if err := queue.Purge(ctx); err != nil {
			// Log but continue
		}
	}

	return result, nil
}

// GetMessagesByTopic returns messages grouped by topic.
func (m *DLQManager) GetMessagesByTopic(ctx context.Context) (map[string]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]int64)
	for _, msg := range m.messages {
		result[msg.Message.Topic]++
	}
	return result, nil
}

// GetMessagesByErrorCode returns messages grouped by error code.
func (m *DLQManager) GetMessagesByErrorCode(ctx context.Context) (map[string]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]int64)
	for _, msg := range m.messages {
		code := msg.ErrorCode
		if code == "" {
			code = "unknown"
		}
		result[code]++
	}
	return result, nil
}

// matchesFilter checks if a DLQ message matches the filter criteria.
func matchesFilter(msg *types.DLQMessage, filter types.DLQFilter) bool {
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

// generateMessageID generates a unique message ID.
func generateMessageID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// extractErrorCode extracts an error code from an error.
func extractErrorCode(err error) string {
	if err == nil {
		return ""
	}

	errStr := err.Error()

	// Try to extract known error types
	if strings.Contains(errStr, "timeout") {
		return "TIMEOUT"
	}
	if strings.Contains(errStr, "connection") {
		return "CONNECTION_ERROR"
	}
	if strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "throttl") {
		return "RATE_LIMITED"
	}
	if strings.Contains(errStr, "not found") {
		return "NOT_FOUND"
	}
	if strings.Contains(errStr, "permission") || strings.Contains(errStr, "unauthorized") {
		return "PERMISSION_DENIED"
	}
	if strings.Contains(errStr, "invalid") {
		return "INVALID_MESSAGE"
	}
	if strings.Contains(errStr, "expired") || strings.Contains(errStr, "TTL") {
		return "EXPIRED"
	}

	// Check for BridgeError
	if be, ok := err.(*types.BridgeError); ok {
		return string(be.Code)
	}

	return "UNKNOWN"
}

// Ensure DLQManager implements types.DLQManager
var _ types.DLQManager = (*DLQManager)(nil)
