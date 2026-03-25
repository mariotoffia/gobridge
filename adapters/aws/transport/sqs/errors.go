package sqs

import (
	"context"
	"errors"
	"strings"

	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

// MapError converts an SQS / AWS SDK error into a *domain.BridgeError
// with the appropriate ErrorClass for the runtime to decide retry vs DLQ.
func MapError(err error) *domain.BridgeError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return domain.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return domain.ErrUnavailable.Wrap(err)
	}

	// Typed SQS API errors.
	var queueNotExist *sqstypes.QueueDoesNotExist
	if errors.As(err, &queueNotExist) {
		return domain.ErrNotFound.Wrap(err).WithMessage("queue does not exist")
	}

	var notInflight *sqstypes.MessageNotInflight
	if errors.As(err, &notInflight) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("message not in flight")
	}

	var badHandle *sqstypes.ReceiptHandleIsInvalid
	if errors.As(err, &badHandle) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("receipt handle is invalid")
	}

	var idsNotDistinct *sqstypes.BatchEntryIdsNotDistinct
	if errors.As(err, &idsNotDistinct) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("batch entry IDs not distinct")
	}

	var emptyBatch *sqstypes.EmptyBatchRequest
	if errors.As(err, &emptyBatch) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("empty batch request")
	}

	var invalidContents *sqstypes.InvalidMessageContents
	if errors.As(err, &invalidContents) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("invalid message contents")
	}

	var tooMany *sqstypes.TooManyEntriesInBatchRequest
	if errors.As(err, &tooMany) {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("too many entries in batch request")
	}

	var batchTooLong *sqstypes.BatchRequestTooLong
	if errors.As(err, &batchTooLong) {
		return domain.ErrPayloadTooLarge.Wrap(err).WithMessage("batch request too long")
	}

	var overLimit *sqstypes.OverLimit
	if errors.As(err, &overLimit) {
		return domain.ErrThrottled.Wrap(err).WithMessage("over limit")
	}

	var unsupported *sqstypes.UnsupportedOperation
	if errors.As(err, &unsupported) {
		return domain.ErrProtocolError.Wrap(err).WithMessage("unsupported operation")
	}

	// Fall back to string-based classification.
	msg := err.Error()

	if containsAny(msg, "throttl", "rate exceeded", "RequestLimitExceeded") {
		return domain.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "ServiceUnavailable", "InternalError", "service unavailable") {
		return domain.ErrUnavailable.Wrap(err)
	}
	if containsAny(msg, "connection refused", "connection reset", "network") {
		return domain.ErrConnectionLost.Wrap(err)
	}
	if containsAny(msg, "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId") {
		return domain.ErrNotAuthorized.Wrap(err)
	}
	if containsAny(msg, "ValidationError", "InvalidParameter", "MalformedQueryString") {
		return domain.ErrInvalidPayload.Wrap(err)
	}

	// Safe default: treat unknown errors as recoverable.
	return domain.ErrUnavailable.Wrap(err)
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
