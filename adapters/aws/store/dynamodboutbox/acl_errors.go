package dynamodboutbox

import (
	"context"
	"errors"
	"strings"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func isTransactionCanceled(err error) bool {
	var tce *ddbtypes.TransactionCanceledException
	return errors.As(err, &tce)
}

func isResourceInUse(err error) bool {
	var riue *ddbtypes.ResourceInUseException
	return errors.As(err, &riue)
}

// mapError classifies a DynamoDB SDK error per the error-wrapping policy
// DDB mapping table.
//
// Per policy Rule 1 (`_design/error-wrapping-policy.adoc:100-104`),
// `context.Canceled` and `context.DeadlineExceeded` are canonical
// sentinels and are returned UNCHANGED (identity-equal) so callers can
// match them with `errors.Is`. They are NEVER reclassified as
// `shared.ErrTimeout` or `shared.ErrUnavailable` — those sentinels are
// reserved for SDK-reported deadline/availability failures.
//
// Caller-handled outcomes (ConditionalCheckFailedException,
// TransactionCanceledException duplicate detection, ResourceInUseException
// idempotent CreateTable) are not classified here — call sites
// short-circuit those before invoking mapError so the outbox-specific
// semantics (ErrDuplicateRecord, ErrStaleFencingToken) are preserved.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// Rule 1: ctx errors pass through verbatim. Do not reclassify.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var notFound *ddbtypes.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return shared.ErrNotFound.Wrap(err)
	}

	var prov *ddbtypes.ProvisionedThroughputExceededException
	if errors.As(err, &prov) {
		return shared.ErrThrottled.Wrap(err)
	}

	var reqLimit *ddbtypes.RequestLimitExceeded
	if errors.As(err, &reqLimit) {
		return shared.ErrThrottled.Wrap(err)
	}

	var internal *ddbtypes.InternalServerError
	if errors.As(err, &internal) {
		return shared.ErrUnavailable.Wrap(err)
	}

	msg := err.Error()
	switch {
	case containsAny(msg, "ThrottlingException", "throttl", "rate exceeded", "RequestLimitExceeded"):
		return shared.ErrThrottled.Wrap(err)
	case containsAny(msg, "ExpiredToken", "expired credentials"):
		return shared.ErrTemporaryAuthFailure.Wrap(err)
	case containsAny(msg, "AccessDenied", "UnauthorizedOperation", "InvalidClientTokenId", "NotAuthorized"):
		return shared.ErrNotAuthorized.Wrap(err)
	case containsAny(msg, "connection refused", "connection reset", "no such host", "network is unreachable"):
		return shared.ErrConnectionLost.Wrap(err)
	case containsAny(msg, "ServiceUnavailable", "InternalError", "service unavailable"):
		return shared.ErrUnavailable.Wrap(err)
	}

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

// wrapErr is the canonical call-site helper for this package. It
// preserves canonical context sentinels (returned identity-equal) and
// otherwise classifies via mapError, attaching the supplied message and
// key/value annotations to the resulting *shared.BridgeError.
//
// kvs must be a sequence of (string-key, value) pairs. An odd-length
// or non-string key is silently ignored to keep error paths panic-free.
func wrapErr(err error, msg string, kvs ...any) error {
	mapped := mapError(err)
	if mapped == nil {
		return nil
	}
	be, ok := mapped.(*shared.BridgeError)
	if !ok {
		// ctx sentinel — return verbatim per policy Rule 1.
		return mapped
	}
	if msg != "" {
		be = be.WithMessage(msg)
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			continue
		}
		be = be.With(key, kvs[i+1])
	}
	return be
}
