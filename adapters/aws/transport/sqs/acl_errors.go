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
		// Receipt-handle expiry: the visibility window lapsed, so SQS
		// already owns redelivery of this message. Classify transient —
		// the failed settlement is a timing conflict, not a poison
		// message; a permanent/rejected class would pollute DLQ
		// categorization and metrics for a condition the broker
		// self-heals.
		return shared.ErrUnavailable.Wrap(err).
			WithMessage("message not in flight (visibility expired; source will redeliver)")
	}

	var badHandle *sqstypes.ReceiptHandleIsInvalid
	if errors.As(err, &badHandle) {
		// Same class as MessageNotInflight: a stale/expired receipt
		// handle means SQS will redeliver on its own — transient, not
		// a permanent payload rejection.
		return shared.ErrUnavailable.Wrap(err).
			WithMessage("receipt handle invalid or expired (source will redeliver)")
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

	// KMS (server-side encryption) errors need code-specific classification
	// (Finding 3). The string fallback below lower-cases the message and
	// matches "KmsAccessDenied" as "accessdenied" → permanent ErrNotAuthorized,
	// which false-DLQs every send during the 10-120s a freshly-granted KMS
	// key policy or IAM role takes to propagate. These typed checks MUST
	// precede the string fallback so the transient KMS codes are not
	// misclassified. Classification follows AWS KMS error semantics.
	var kmsThrottled *sqstypes.KmsThrottled
	if errors.As(err, &kmsThrottled) {
		// KMS-side request throttling — transient, back off and retry.
		return shared.ErrThrottled.Wrap(err).WithMessage("kms throttled")
	}
	var kmsAccessDenied *sqstypes.KmsAccessDenied
	if errors.As(err, &kmsAccessDenied) {
		// Caller lacks KMS access. Classify TEMPORARY: a freshly-granted key
		// policy / IAM role commonly takes 10-120s to propagate and a
		// permanent class would DLQ every message in that window. If the
		// grant never lands, the runtime's retry budget still bounds it.
		return shared.ErrTemporaryAuthFailure.Wrap(err).
			WithMessage("kms access denied (grant may still be propagating)")
	}
	// The remaining KMS codes are genuine, non-self-healing misconfigurations
	// (disabled/pending-deletion key, wrong key spec, missing key, region
	// opt-in): retrying cannot succeed within a redelivery window, so classify
	// permanent and let the runtime DLQ.
	var kmsDisabled *sqstypes.KmsDisabled
	if errors.As(err, &kmsDisabled) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key disabled")
	}
	var kmsInvalidState *sqstypes.KmsInvalidState
	if errors.As(err, &kmsInvalidState) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key invalid state")
	}
	var kmsNotFound *sqstypes.KmsNotFound
	if errors.As(err, &kmsNotFound) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key not found")
	}
	var kmsOptIn *sqstypes.KmsOptInRequired
	if errors.As(err, &kmsOptIn) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms opt-in required")
	}
	var kmsInvalidKeyUsage *sqstypes.KmsInvalidKeyUsage
	if errors.As(err, &kmsInvalidKeyUsage) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms invalid key usage")
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
	// Plain (non-KMS) API auth failures. NOTE — Finding 3 residual: a fresh
	// IAM role can take 10-120s to propagate, during which SendMessage
	// returns AccessDenied for a condition that WILL self-heal. Ideally the
	// first N encounters would classify temporary, but MapError is a
	// stateless pure function with no per-message retry counter, so it cannot
	// distinguish "still propagating" from "genuinely misconfigured" without
	// threading retry state through the ACL. Plain API auth therefore stays
	// PERMANENT (a truly-misconfigured policy DLQs instead of retrying
	// forever); only the code-distinguishable KmsAccessDenied above is mapped
	// temporary.
	if containsAny(msg, "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId") {
		return shared.ErrNotAuthorized.Wrap(err)
	}
	// MissingParameter covers deterministic request faults such as a
	// FIFO send without a MessageGroupId: retrying cannot succeed, so
	// it must classify rejected — the same class the batch API's
	// SenderFault yields — instead of falling through to the transient
	// default and being retried forever.
	if containsAny(msg, "ValidationError", "InvalidParameter", "MissingParameter", "MalformedQueryString") {
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
