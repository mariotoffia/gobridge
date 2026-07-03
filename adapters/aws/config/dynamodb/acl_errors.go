package dynamodb

import (
	"errors"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	smithy "github.com/aws/smithy-go"
)

// DynamoDB SDK error classification.
//
// This file is the error-classification half of the dynamodb config
// ACL: it owns the only references to ddbtypes for error sentinel
// checks, plus the smithy API-code classification the streams consumer
// uses to decide between "keep the iterator and back off" and "the
// iterator is gone, re-acquire and reconcile".

// mapEnsureTableError swallows ResourceInUseException (the table
// already exists, which is the success outcome for an idempotent
// EnsureTable call). All other errors are returned unchanged for the
// caller to wrap.
func mapEnsureTableError(err error) error {
	if err == nil {
		return nil
	}
	var inUse *ddbtypes.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	return err
}

// isConditionFailed reports whether err is a DynamoDB
// ConditionalCheckFailedException, i.e. a compare-and-set conflict on a
// conditional write (a concurrent Save advanced the version).
func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// isStreamIteratorInvalid reports whether err means the shard iterator
// can no longer make progress and must be re-acquired: it expired
// (iterators live 15 minutes), the requested position was trimmed out
// of the 24h retention window, or the stream/shard no longer exists.
// Classified via the protocol-agnostic smithy API code so this file
// stays free of dynamodbstreams type imports (those are owned by
// acl_streams.go).
func isStreamIteratorInvalid(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "ExpiredIteratorException",
		"TrimmedDataAccessException",
		"ResourceNotFoundException":
		return true
	default:
		return false
	}
}

// isStreamThrottle reports whether err is a rate/capacity rejection on
// the streams API. GetRecords shares a ~5 TPS per-shard budget across
// ALL stream consumers, so throttles are expected in clustered
// deployments; the consumer must back off but keep its iterator so no
// stream position (and hence no config update) is lost.
func isStreamThrottle(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "LimitExceededException",
		"ProvisionedThroughputExceededException",
		"ThrottlingException",
		"RequestLimitExceeded":
		return true
	default:
		return false
	}
}
