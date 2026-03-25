// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Error Mapping Unit Tests
//
// Tests for MapError function that converts AWS/SQS errors to BridgeError.
//
// Error Classification:
// ┌─────────────────────────────┬───────────────────┬──────────────────┐
// │ AWS/SQS Error               │ BridgeError       │ IsRecoverable    │
// ├─────────────────────────────┼───────────────────┼──────────────────┤
// │ QueueDoesNotExist           │ ErrNotFound       │ false            │
// │ MessageNotInflight          │ ErrInvalidPayload │ false            │
// │ ReceiptHandleIsInvalid      │ ErrInvalidPayload │ false            │
// │ BatchEntryIdsNotDistinct    │ ErrInvalidPayload │ false            │
// │ EmptyBatchRequest           │ ErrInvalidPayload │ false            │
// │ InvalidMessageContents      │ ErrInvalidPayload │ false            │
// │ TooManyEntriesInBatchRequest│ ErrInvalidPayload │ false            │
// │ BatchRequestTooLong         │ ErrPayloadTooLarge│ false            │
// │ OverLimit                   │ ErrThrottled      │ true             │
// │ UnsupportedOperation        │ ErrProtocolError  │ false            │
// │ context.DeadlineExceeded    │ ErrTimeout        │ true             │
// │ context.Canceled            │ ErrUnavailable    │ true             │
// │ "throttl..."                │ ErrThrottled      │ true             │
// │ "ServiceUnavailable"        │ ErrUnavailable    │ true             │
// │ "connection refused"        │ ErrConnectionLost │ true             │
// │ "AccessDenied"              │ ErrNotAuthorized  │ false            │
// │ "ValidationError"           │ ErrInvalidPayload │ false            │
// │ (unknown)                   │ ErrUnavailable    │ true (safe)      │
// └─────────────────────────────┴───────────────────┴──────────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ E001 │ MapError nil returns nil               │ PASS     │
// │ E002 │ MapError context.DeadlineExceeded      │ PASS     │
// │ E003 │ MapError context.Canceled              │ PASS     │
// │ E004 │ MapError QueueDoesNotExist             │ PASS     │
// │ E005 │ MapError MessageNotInflight            │ PASS     │
// │ E006 │ MapError ReceiptHandleIsInvalid        │ PASS     │
// │ E007 │ MapError BatchEntryIdsNotDistinct      │ PASS     │
// │ E008 │ MapError EmptyBatchRequest             │ PASS     │
// │ E009 │ MapError InvalidMessageContents        │ PASS     │
// │ E010 │ MapError TooManyEntriesInBatchRequest  │ PASS     │
// │ E011 │ MapError BatchRequestTooLong           │ PASS     │
// │ E012 │ MapError OverLimit                     │ PASS     │
// │ E013 │ MapError UnsupportedOperation          │ PASS     │
// │ E014 │ MapError throttling patterns           │ PASS     │
// │ E015 │ MapError unavailable patterns          │ PASS     │
// │ E016 │ MapError network error patterns        │ PASS     │
// │ E017 │ MapError auth error patterns           │ PASS     │
// │ E018 │ MapError validation error patterns     │ PASS     │
// │ E019 │ MapError unknown defaults recoverable  │ PASS     │
// │ E020 │ MapError wraps original error          │ PASS     │
// │ E021 │ containsAny case insensitive           │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqstests

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// MapError Basic Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_Nil validates nil input returns nil.
func TestMapError_Nil(t *testing.T) {
	result := sqs.MapError(nil)
	assert.Nil(t, result)
}

// TestMapError_ContextDeadlineExceeded validates context timeout mapping.
func TestMapError_ContextDeadlineExceeded(t *testing.T) {
	err := sqs.MapError(context.DeadlineExceeded)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrTimeout))
	assert.True(t, err.IsRecoverable, "timeout should be recoverable")
}

// TestMapError_ContextCanceled validates context canceled mapping.
func TestMapError_ContextCanceled(t *testing.T) {
	err := sqs.MapError(context.Canceled)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrUnavailable))
	assert.True(t, err.IsRecoverable, "canceled should be recoverable")
}

// ═══════════════════════════════════════════════════════════════════════════
// SQS-Specific Error Type Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_QueueDoesNotExist validates queue not found mapping.
func TestMapError_QueueDoesNotExist(t *testing.T) {
	sqsErr := &types.QueueDoesNotExist{Message: stringPtr("Queue not found")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrNotFound))
	assert.False(t, err.IsRecoverable, "not found should be permanent")
	assert.Contains(t, err.Message, "queue does not exist")
}

// TestMapError_MessageNotInflight validates message not inflight mapping.
func TestMapError_MessageNotInflight(t *testing.T) {
	sqsErr := &types.MessageNotInflight{Message: stringPtr("Message not in flight")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable, "message not inflight should be permanent")
}

// TestMapError_ReceiptHandleIsInvalid validates invalid receipt handle mapping.
func TestMapError_ReceiptHandleIsInvalid(t *testing.T) {
	sqsErr := &types.ReceiptHandleIsInvalid{Message: stringPtr("Invalid receipt handle")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable)
}

// TestMapError_BatchEntryIdsNotDistinct validates batch ID error mapping.
func TestMapError_BatchEntryIdsNotDistinct(t *testing.T) {
	sqsErr := &types.BatchEntryIdsNotDistinct{Message: stringPtr("Duplicate IDs")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable)
}

// TestMapError_EmptyBatchRequest validates empty batch error mapping.
func TestMapError_EmptyBatchRequest(t *testing.T) {
	sqsErr := &types.EmptyBatchRequest{Message: stringPtr("Empty batch")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable)
}

// TestMapError_InvalidMessageContents validates invalid message mapping.
func TestMapError_InvalidMessageContents(t *testing.T) {
	sqsErr := &types.InvalidMessageContents{Message: stringPtr("Invalid contents")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable)
}

// TestMapError_TooManyEntriesInBatchRequest validates too many entries mapping.
func TestMapError_TooManyEntriesInBatchRequest(t *testing.T) {
	sqsErr := &types.TooManyEntriesInBatchRequest{Message: stringPtr("Too many entries")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
	assert.False(t, err.IsRecoverable)
}

// TestMapError_BatchRequestTooLong validates batch too long mapping.
func TestMapError_BatchRequestTooLong(t *testing.T) {
	sqsErr := &types.BatchRequestTooLong{Message: stringPtr("Batch too long")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrPayloadTooLarge))
	assert.False(t, err.IsRecoverable, "payload too large should be permanent")
}

// TestMapError_OverLimit validates throttling mapping.
func TestMapError_OverLimit(t *testing.T) {
	sqsErr := &types.OverLimit{Message: stringPtr("Over limit")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrThrottled))
	assert.True(t, err.IsRecoverable, "throttled should be recoverable")
}

// TestMapError_UnsupportedOperation validates unsupported operation mapping.
func TestMapError_UnsupportedOperation(t *testing.T) {
	sqsErr := &types.UnsupportedOperation{Message: stringPtr("Unsupported")}
	err := sqs.MapError(sqsErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrProtocolError))
	assert.False(t, err.IsRecoverable, "protocol error should be permanent")
}

// ═══════════════════════════════════════════════════════════════════════════
// Error Pattern Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_ThrottlingPatterns validates throttling error message patterns.
func TestMapError_ThrottlingPatterns(t *testing.T) {
	tests := []struct {
		name    string
		errMsg  string
		isMatch bool
	}{
		{"throttling lowercase", "request throttling", true},
		{"Throttling capitalized", "Throttling exception", true},
		{"rate exceeded", "rate exceeded for queue", true},
		{"Rate Exceeded capitalized", "Rate Exceeded", true},
		{"RequestLimitExceeded", "RequestLimitExceeded", true},
		{"unrelated", "some other error", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))

			if tc.isMatch {
				assert.True(t, errors.Is(err, bridgeTypes.ErrThrottled),
					"should match throttling pattern")
				assert.True(t, err.IsRecoverable)
			}
		})
	}
}

// TestMapError_UnavailablePatterns validates service unavailable patterns.
func TestMapError_UnavailablePatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"ServiceUnavailable", "ServiceUnavailable"},
		{"InternalError", "InternalError occurred"},
		{"service unavailable lowercase", "service unavailable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))

			assert.True(t, errors.Is(err, bridgeTypes.ErrUnavailable))
			assert.True(t, err.IsRecoverable)
		})
	}
}

// TestMapError_NetworkErrorPatterns validates network error patterns.
func TestMapError_NetworkErrorPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"connection refused", "dial tcp: connection refused"},
		{"connection reset", "connection reset by peer"},
		{"network error", "network unreachable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))

			assert.True(t, errors.Is(err, bridgeTypes.ErrConnectionLost))
			assert.True(t, err.IsRecoverable)
		})
	}
}

// TestMapError_AuthErrorPatterns validates authentication error patterns.
func TestMapError_AuthErrorPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"AccessDenied", "AccessDenied: User not authorized"},
		{"UnauthorizedAccess", "UnauthorizedAccess exception"},
		{"InvalidClientTokenId", "InvalidClientTokenId"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))

			assert.True(t, errors.Is(err, bridgeTypes.ErrNotAuthorized))
			assert.False(t, err.IsRecoverable, "auth errors should be permanent")
		})
	}
}

// TestMapError_ValidationErrorPatterns validates validation error patterns.
func TestMapError_ValidationErrorPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"ValidationError", "ValidationError: invalid parameter"},
		{"InvalidParameter", "InvalidParameterValue"},
		{"MalformedQueryString", "MalformedQueryString"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))

			assert.True(t, errors.Is(err, bridgeTypes.ErrInvalidPayload))
			assert.False(t, err.IsRecoverable, "validation errors should be permanent")
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Default and Edge Case Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_UnknownDefaultsRecoverable validates unknown errors default to recoverable.
func TestMapError_UnknownDefaultsRecoverable(t *testing.T) {
	unknownErr := errors.New("some completely unknown error")
	err := sqs.MapError(unknownErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrUnavailable),
		"unknown errors should map to ErrUnavailable")
	assert.True(t, err.IsRecoverable,
		"unknown errors should be recoverable (safe default)")
}

// TestMapError_WrapsOriginalError validates original error is wrapped.
func TestMapError_WrapsOriginalError(t *testing.T) {
	originalErr := errors.New("original error message")
	err := sqs.MapError(originalErr)

	assert.NotNil(t, err)
	assert.True(t, errors.Is(err.Wrapped, originalErr) || err.Wrapped == originalErr,
		"original error should be wrapped")
}

// ═══════════════════════════════════════════════════════════════════════════
// Helper Function Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContainsAny_CaseInsensitive validates case-insensitive matching.
//
// Note: containsAny is not exported, so we test indirectly via MapError patterns.
func TestContainsAny_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name    string
		errMsg  string
		isMatch bool
	}{
		{"lowercase throttling", "throttling limit", true},
		{"uppercase THROTTLING", "THROTTLING LIMIT", true},
		{"mixed ThRoTtLiNg", "ThRoTtLiNg LiMiT", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sqs.MapError(errors.New(tc.errMsg))
			assert.True(t, errors.Is(err, bridgeTypes.ErrThrottled),
				"should match case-insensitively")
		})
	}
}

// TestContainsAny_NoMatch validates no false positives.
func TestContainsAny_NoMatch(t *testing.T) {
	// This error message doesn't match any known patterns
	err := sqs.MapError(errors.New("xyz123"))

	// Should default to ErrUnavailable
	assert.True(t, errors.Is(err, bridgeTypes.ErrUnavailable))
}

// ═══════════════════════════════════════════════════════════════════════════
// Helper for AWS String Pointers
// ═══════════════════════════════════════════════════════════════════════════

// stringPtr creates a string pointer (mimics aws.String).
func stringPtr(s string) *string {
	return &s
}
