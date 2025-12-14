package sqs

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
)

// MapError converts SQS/AWS errors to BridgeError with correct classification.
func MapError(err error) *bridgeTypes.BridgeError {
	if err == nil {
		return nil
	}

	// Context errors
	if errors.Is(err, context.DeadlineExceeded) {
		return bridgeTypes.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return bridgeTypes.ErrUnavailable.Wrap(err)
	}

	// Check for specific SQS errors
	var queueDoesNotExist *types.QueueDoesNotExist
	if errors.As(err, &queueDoesNotExist) {
		return bridgeTypes.ErrNotFound.
			Wrap(err).
			WithMessage("queue does not exist")
	}

	var messageNotInflight *types.MessageNotInflight
	if errors.As(err, &messageNotInflight) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("message not in flight")
	}

	var receiptHandleIsInvalid *types.ReceiptHandleIsInvalid
	if errors.As(err, &receiptHandleIsInvalid) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("receipt handle is invalid")
	}

	var batchEntryIdsNotDistinct *types.BatchEntryIdsNotDistinct
	if errors.As(err, &batchEntryIdsNotDistinct) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("batch entry IDs not distinct")
	}

	var emptyBatchRequest *types.EmptyBatchRequest
	if errors.As(err, &emptyBatchRequest) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("empty batch request")
	}

	var invalidMessageContents *types.InvalidMessageContents
	if errors.As(err, &invalidMessageContents) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("invalid message contents")
	}

	var tooManyEntriesInBatchRequest *types.TooManyEntriesInBatchRequest
	if errors.As(err, &tooManyEntriesInBatchRequest) {
		return bridgeTypes.ErrInvalidPayload.
			Wrap(err).
			WithMessage("too many entries in batch request")
	}

	var batchRequestTooLong *types.BatchRequestTooLong
	if errors.As(err, &batchRequestTooLong) {
		return bridgeTypes.ErrPayloadTooLarge.
			Wrap(err).
			WithMessage("batch request too long")
	}

	var overLimit *types.OverLimit
	if errors.As(err, &overLimit) {
		return bridgeTypes.ErrThrottled.
			Wrap(err).
			WithMessage("over limit")
	}

	var unsupportedOperation *types.UnsupportedOperation
	if errors.As(err, &unsupportedOperation) {
		return bridgeTypes.ErrProtocolError.
			Wrap(err).
			WithMessage("unsupported operation")
	}

	// Check error message patterns
	errStr := err.Error()

	// Throttling errors - recoverable
	if containsAny(errStr, "throttl", "rate exceeded", "RequestLimitExceeded") {
		return bridgeTypes.ErrThrottled.Wrap(err)
	}

	// Service unavailable - recoverable
	if containsAny(errStr, "ServiceUnavailable", "InternalError", "service unavailable") {
		return bridgeTypes.ErrUnavailable.Wrap(err)
	}

	// Network errors - recoverable
	if containsAny(errStr, "connection refused", "connection reset", "network") {
		return bridgeTypes.ErrConnectionLost.Wrap(err)
	}

	// Auth errors - permanent
	if containsAny(errStr, "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId") {
		return bridgeTypes.ErrNotAuthorized.Wrap(err)
	}

	// Validation errors - permanent
	if containsAny(errStr, "ValidationError", "InvalidParameter", "MalformedQueryString") {
		return bridgeTypes.ErrInvalidPayload.Wrap(err)
	}

	// Default: treat as recoverable (safe default)
	return bridgeTypes.ErrUnavailable.Wrap(err)
}

// containsAny checks if s contains any of the substrings (case-insensitive).
func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

