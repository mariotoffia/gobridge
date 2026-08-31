package dynamodboutbox

import (
	"context"
	"errors"
	"strings"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/shared"
)

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// transactCancellationCodes returns the per-item cancellation reason codes
// of a TransactionCanceledException ("ConditionalCheckFailed",
// "TransactionConflict", "None", ...) in TransactItems order, and whether
// err was a transaction cancellation at all. Callers map the codes onto the
// items they submitted (e.g. fence ConditionCheck at index 0, record
// update at index 1 in claimOne).
func transactCancellationCodes(err error) ([]string, bool) {
	var tce *ddbtypes.TransactionCanceledException
	if !errors.As(err, &tce) {
		return nil, false
	}
	codes := make([]string, len(tce.CancellationReasons))
	for i, r := range tce.CancellationReasons {
		if r.Code != nil {
			codes[i] = *r.Code
		}
	}
	return codes, true
}

// Per-item TransactWriteItems cancellation reason codes. Only these three are
// BENIGN contention on a claim: a non-cause item (None), a lost claim race
// (ConditionalCheckFailed), or a concurrent write on the same item
// (TransactionConflict). Any OTHER reason — ProvisionedThroughputExceeded,
// ThrottlingError, ValidationError, ... — is a real fault that must surface,
// never be swallowed as a silent skip (c13-txn-throttle).
const (
	ccReasonNone            = "None"
	ccReasonCondCheckFailed = "ConditionalCheckFailed"
	ccReasonTxnConflict     = "TransactionConflict"
)

// nonContentionCancellation reports whether a transaction's per-item
// cancellation reason codes include any reason OUTSIDE the benign contention
// set (None / ConditionalCheckFailed / TransactionConflict). Such a reason
// means the claim did not merely lose a race: a throttle
// (ProvisionedThroughputExceeded, ThrottlingError) must reach the drainer as
// a retryable error so it BACKS OFF, and a permanent fault (ValidationError)
// must surface honestly — either way the record must NOT be dropped as a
// silent skip. It returns the first offending code for diagnostics.
func nonContentionCancellation(codes []string) (string, bool) {
	for _, c := range codes {
		switch c {
		case "", ccReasonNone, ccReasonCondCheckFailed, ccReasonTxnConflict:
			continue
		default:
			return c, true
		}
	}
	return "", false
}

// classifyCancellationCodes maps the first non-contention transaction
// cancellation reason code to the module's canonical error class, so
// wrapErr/mapError surface a throttled claim as retryable shared.ErrThrottled
// (drainer backs off) and a permanent fault as shared.ErrInvalidPayload
// (never retried forever). Benign contention codes (handled and
// short-circuited at the call site) and unrecognised codes yield nil here so
// mapError falls through to its generic classification (a safe transient
// default), never to a silent skip.
func classifyCancellationCodes(codes []string, err error) error {
	for _, c := range codes {
		switch c {
		case "ProvisionedThroughputExceeded", "ThrottlingError", "ThrottlingException", "RequestLimitExceeded":
			return shared.ErrThrottled.Wrap(err)
		case "ValidationError", "ValidationException", "ItemCollectionSizeLimitExceeded":
			return shared.ErrInvalidPayload.Wrap(err)
		case "InternalServerError":
			return shared.ErrUnavailable.Wrap(err)
		case "AccessDenied", "AccessDeniedException":
			return shared.ErrNotAuthorized.Wrap(err)
		}
	}
	return nil
}

// hasCode reports whether any per-item cancellation reason equals code (e.g.
// "TransactionConflict"). Used to distinguish contention (a concurrent write
// touched the same item) from a benign lost race on the record condition.
func hasCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
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

	// A TransactionCanceledException that reaches mapError is NOT a benign
	// contention cancellation — claimOne short-circuits the fence conflict and
	// the ConditionalCheckFailed/TransactionConflict lost-race codes before
	// wrapping. What remains carries a real fault reason (throttle, throughput,
	// validation), which must be classified honestly so the drainer backs off
	// or stops retrying instead of silently skipping the record
	// (c13-txn-throttle).
	if codes, ok := transactCancellationCodes(err); ok {
		if mapped := classifyCancellationCodes(codes, err); mapped != nil {
			return mapped
		}
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

	// ValidationException is deterministic request rejection — most
	// prominently an item exceeding DynamoDB's 400KB limit (oversized
	// envelope). It is a PERMANENT failure: retrying the identical write
	// can never succeed, so it must not be classified transient (which
	// would retry an unpersistable record forever).
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException" {
		return shared.ErrInvalidPayload.Wrap(err)
	}

	msg := err.Error()
	switch {
	case containsAny(msg, "ValidationException"):
		return shared.ErrInvalidPayload.Wrap(err)
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
