package types

import (
	"context"
	"time"
)

// ============================================================================
// DLQ Manager Interface
// ============================================================================

// DLQManager provides a high-level API for managing dead letter queues.
// It wraps one or more DeadLetterQueue implementations and provides
// additional functionality like statistics, message inspection, and replay.
type DLQManager interface {
	// GetSummary returns a summary of all DLQ statistics.
	GetSummary(ctx context.Context) (*DLQSummary, error)

	// ListMessages returns messages from the DLQ with optional filtering.
	ListMessages(ctx context.Context, filter DLQFilter, pagination DLQPagination) (*DLQMessageList, error)

	// GetMessage returns a specific message by ID.
	GetMessage(ctx context.Context, messageID string) (*DLQMessage, error)

	// DeleteMessage removes a specific message from the DLQ.
	DeleteMessage(ctx context.Context, messageID string) error

	// Replay moves messages from DLQ back to the retry queue.
	Replay(ctx context.Context, request *DLQReplayRequest) (*DLQReplayResult, error)

	// Purge removes messages from the DLQ based on filter criteria.
	Purge(ctx context.Context, filter DLQFilter) (*DLQPurgeResult, error)

	// GetMessagesByTopic returns messages grouped by topic.
	GetMessagesByTopic(ctx context.Context) (map[string]int64, error)

	// GetMessagesByErrorCode returns messages grouped by error code.
	GetMessagesByErrorCode(ctx context.Context) (map[string]int64, error)

	// RegisterQueue registers a DLQ for a pipeline.
	RegisterQueue(pipelineID string, queue DeadLetterQueue)

	// GetQueue returns the DLQ for a pipeline.
	GetQueue(pipelineID string) (DeadLetterQueue, bool)
}

// DLQSummary contains aggregate statistics for all DLQs.
type DLQSummary struct {
	// TotalMessages is the total number of messages in all DLQs.
	TotalMessages int64 `json:"totalMessages"`
	// OldestMessage is the timestamp of the oldest message.
	OldestMessage *time.Time `json:"oldestMessage,omitempty"`
	// NewestMessage is the timestamp of the newest message.
	NewestMessage *time.Time `json:"newestMessage,omitempty"`
	// ByPipeline shows message counts per pipeline.
	ByPipeline map[string]int64 `json:"byPipeline,omitempty"`
	// ByTopic shows message counts per topic.
	ByTopic map[string]int64 `json:"byTopic,omitempty"`
	// ByErrorCode shows message counts per error code.
	ByErrorCode map[string]int64 `json:"byErrorCode,omitempty"`
}

// DLQPagination controls pagination for message listing.
type DLQPagination struct {
	// Limit is the maximum number of messages to return.
	Limit int `json:"limit"`
	// Offset is the number of messages to skip.
	Offset int `json:"offset"`
}

// DLQMessageList is a paginated list of DLQ messages.
type DLQMessageList struct {
	// Messages are the returned messages.
	Messages []*DLQMessage `json:"messages"`
	// Total is the total number of messages matching the filter.
	Total int64 `json:"total"`
	// Offset is the current offset.
	Offset int `json:"offset"`
	// Limit is the current limit.
	Limit int `json:"limit"`
	// HasMore indicates if there are more messages.
	HasMore bool `json:"hasMore"`
}

// DLQReplayRequest specifies which messages to replay.
type DLQReplayRequest struct {
	// MessageIDs are specific message IDs to replay.
	// If empty, uses Filter to select messages.
	MessageIDs []string `json:"messageIds,omitempty"`
	// Filter selects messages to replay if MessageIDs is empty.
	Filter DLQFilter `json:"filter,omitempty"`
	// TargetPipeline optionally specifies a different pipeline for replay.
	// If empty, messages are replayed to their original pipeline.
	TargetPipeline string `json:"targetPipeline,omitempty"`
	// ResetRetryCount resets the retry count when replaying.
	ResetRetryCount bool `json:"resetRetryCount,omitempty"`
}

// DLQReplayResult contains the result of a replay operation.
type DLQReplayResult struct {
	// Replayed is the number of messages successfully replayed.
	Replayed int64 `json:"replayed"`
	// Failed is the number of messages that failed to replay.
	Failed int64 `json:"failed"`
	// Errors contains error messages for failed replays.
	Errors []string `json:"errors,omitempty"`
	// ReplayedIDs are the IDs of successfully replayed messages.
	ReplayedIDs []string `json:"replayedIds,omitempty"`
}

// DLQPurgeResult contains the result of a purge operation.
type DLQPurgeResult struct {
	// Purged is the number of messages purged.
	Purged int64 `json:"purged"`
}

// ============================================================================
// Extended DLQ Message with ID
// ============================================================================

// DLQMessageWithID extends DLQMessage with a unique identifier.
type DLQMessageWithID struct {
	DLQMessage
	// ID is the unique message identifier.
	ID string `json:"id"`
	// PipelineID is the pipeline this message came from.
	PipelineID string `json:"pipelineId"`
	// ErrorCode is a categorized error code.
	ErrorCode string `json:"errorCode,omitempty"`
	// PayloadPreview is a truncated preview of the payload.
	PayloadPreview string `json:"payloadPreview,omitempty"`
}

// ============================================================================
// Config Watcher Interface
// ============================================================================

// ConfigWatcher extends ConfigSource to support watching for changes.
type ConfigWatcher interface {
	ConfigSource
	// Watch returns a channel that receives configuration changes.
	Watch(ctx context.Context) (<-chan ConfigChange, error)
}
