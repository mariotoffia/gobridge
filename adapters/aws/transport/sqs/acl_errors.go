package sqs

import (
	"context"
	"errors"
	"strings"

	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts an SQS / AWS SDK error into a *shared.BridgeError
// with the appropriate ErrorClass for the runtime to decide retry vs DLQ.
func MapError(err error) *shared.BridgeError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return shared.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return shared.ErrUnavailable.Wrap(err)
	}

	// Typed SQS API errors.
	var queueNotExist *sqstypes.QueueDoesNotExist
	if errors.As(err, &queueNotExist) {
		return shared.ErrNotFound.Wrap(err).WithMessage("queue does not exist")
	}

	var notInflight *sqstypes.MessageNotInflight
	if errors.As(err, &notInflight) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("message not in flight")
	}

	var badHandle *sqstypes.ReceiptHandleIsInvalid
	if errors.As(err, &badHandle) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("receipt handle is invalid")
	}

	var idsNotDistinct *sqstypes.BatchEntryIdsNotDistinct
	if errors.As(err, &idsNotDistinct) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("batch entry IDs not distinct")
	}

	var emptyBatch *sqstypes.EmptyBatchRequest
	if errors.As(err, &emptyBatch) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("empty batch request")
	}

	var invalidContents *sqstypes.InvalidMessageContents
	if errors.As(err, &invalidContents) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("invalid message contents")
	}

	var tooMany *sqstypes.TooManyEntriesInBatchRequest
	if errors.As(err, &tooMany) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("too many entries in batch request")
	}

	var batchTooLong *sqstypes.BatchRequestTooLong
	if errors.As(err, &batchTooLong) {
		return shared.ErrPayloadTooLarge.Wrap(err).WithMessage("batch request too long")
	}

	var overLimit *sqstypes.OverLimit
	if errors.As(err, &overLimit) {
		return shared.ErrThrottled.Wrap(err).WithMessage("over limit")
	}

	var unsupported *sqstypes.UnsupportedOperation
	if errors.As(err, &unsupported) {
		return shared.ErrProtocolError.Wrap(err).WithMessage("unsupported operation")
	}

	// Fall back to string-based classification.
	msg := err.Error()

	if containsAny(msg, "throttl", "rate exceeded", "RequestLimitExceeded") {
		return shared.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "ServiceUnavailable", "InternalError", "service unavailable") {
		return shared.ErrUnavailable.Wrap(err)
	}
	if containsAny(msg, "connection refused", "connection reset", "network") {
		return shared.ErrConnectionLost.Wrap(err)
	}
	if containsAny(msg, "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId") {
		return shared.ErrNotAuthorized.Wrap(err)
	}
	if containsAny(msg, "ValidationError", "InvalidParameter", "MalformedQueryString") {
		return shared.ErrInvalidPayload.Wrap(err)
	}

	// Safe default: treat unknown errors as recoverable.
	return shared.ErrUnavailable.Wrap(err)
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
