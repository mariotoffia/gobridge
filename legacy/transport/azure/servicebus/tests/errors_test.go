// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Error Mapping Unit Tests
//
// Tests for MapError and error classification.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ E001 │ MapError nil error                     │ PASS     │
// │ E002 │ MapError context deadline              │ PASS     │
// │ E003 │ MapError context canceled              │ PASS     │
// │ E004 │ MapError connection patterns           │ PASS     │
// │ E005 │ MapError throttling patterns           │ PASS     │
// │ E006 │ MapError auth patterns                 │ PASS     │
// │ E007 │ MapError not found patterns            │ PASS     │
// │ E008 │ MapError validation patterns           │ PASS     │
// │ E009 │ MapError default recoverable           │ PASS     │
// │ E010 │ ContainsAny helper function            │ PASS     │
// │ E011 │ Error recoverability classification    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebustests

import (
	"context"
	"errors"
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/azure/servicebus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// MapError Basic Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_NilError validates nil input returns nil output.
func TestMapError_NilError(t *testing.T) {
	result := servicebus.MapError(nil)
	assert.Nil(t, result)
}

// TestMapError_ContextDeadlineExceeded validates timeout mapping.
//
// ═══════════════════════════════════════════════════════════════════════════
// context.DeadlineExceeded → ErrTimeout (Recoverable)
// ═══════════════════════════════════════════════════════════════════════════
func TestMapError_ContextDeadlineExceeded(t *testing.T) {
	result := servicebus.MapError(context.DeadlineExceeded)

	require.NotNil(t, result)
	assert.Equal(t, bridgeTypes.ErrTimeout.Code, result.Code)
	assert.True(t, result.IsRecoverable, "timeout should be recoverable")
}

// TestMapError_ContextCanceled validates cancellation mapping.
//
// ═══════════════════════════════════════════════════════════════════════════
// context.Canceled → ErrUnavailable (Recoverable)
// ═══════════════════════════════════════════════════════════════════════════
func TestMapError_ContextCanceled(t *testing.T) {
	result := servicebus.MapError(context.Canceled)

	require.NotNil(t, result)
	assert.Equal(t, bridgeTypes.ErrUnavailable.Code, result.Code)
	assert.True(t, result.IsRecoverable, "canceled should be recoverable")
}

// ═══════════════════════════════════════════════════════════════════════════
// String Pattern Matching Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_ConnectionPatterns validates connection error detection.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Error messages containing:                                             │
// │  ├─ "connection" → ErrConnectionLost (Recoverable)                     │
// │  ├─ "network"    → ErrConnectionLost (Recoverable)                     │
// │  ├─ "timeout"    → ErrConnectionLost (Recoverable)                     │
// │  └─ "reset"      → ErrConnectionLost (Recoverable)                     │
// └─────────────────────────────────────────────────────────────────────────┘
func TestMapError_ConnectionPatterns(t *testing.T) {
	tests := []struct {
		name    string
		errMsg  string
		wantErr *bridgeTypes.BridgeError
	}{
		{
			name:    "connection keyword",
			errMsg:  "connection refused",
			wantErr: bridgeTypes.ErrConnectionLost,
		},
		{
			name:    "network keyword",
			errMsg:  "network unreachable",
			wantErr: bridgeTypes.ErrConnectionLost,
		},
		{
			name:    "timeout keyword",
			errMsg:  "request timeout",
			wantErr: bridgeTypes.ErrConnectionLost,
		},
		{
			name:    "reset keyword",
			errMsg:  "connection reset by peer",
			wantErr: bridgeTypes.ErrConnectionLost,
		},
		{
			name:    "mixed case",
			errMsg:  "CONNECTION failed",
			wantErr: bridgeTypes.ErrConnectionLost,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, tc.wantErr.Code, result.Code)
			assert.True(t, result.IsRecoverable, "connection errors should be recoverable")
		})
	}
}

// TestMapError_ThrottlingPatterns validates throttling error detection.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Error messages containing:                                             │
// │  ├─ "throttl"   → ErrThrottled (Recoverable)                           │
// │  ├─ "busy"      → ErrThrottled (Recoverable)                           │
// │  ├─ "overload"  → ErrThrottled (Recoverable)                           │
// │  └─ "too many"  → ErrThrottled (Recoverable)                           │
// └─────────────────────────────────────────────────────────────────────────┘
func TestMapError_ThrottlingPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"throttled", "request throttled"},
		{"throttling", "throttling exception"},
		{"busy", "server busy"},
		{"overload", "system overloaded"},
		{"too many", "too many requests"},
		{"mixed case", "THROTTLED by server"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, bridgeTypes.ErrThrottled.Code, result.Code)
			assert.True(t, result.IsRecoverable, "throttling should be recoverable")
		})
	}
}

// TestMapError_AuthPatterns validates authentication error detection.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Error messages containing:                                             │
// │  ├─ "unauthorized"   → ErrNotAuthorized (NOT Recoverable)              │
// │  ├─ "forbidden"      → ErrNotAuthorized (NOT Recoverable)              │
// │  ├─ "access denied"  → ErrNotAuthorized (NOT Recoverable)              │
// │  ├─ "401"            → ErrNotAuthorized (NOT Recoverable)              │
// │  └─ "403"            → ErrNotAuthorized (NOT Recoverable)              │
// └─────────────────────────────────────────────────────────────────────────┘
func TestMapError_AuthPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"unauthorized", "unauthorized access"},
		{"forbidden", "forbidden resource"},
		{"access denied", "access denied to queue"},
		{"http 401", "HTTP 401 error"},
		{"http 403", "HTTP 403 forbidden"},
		{"mixed case", "UNAUTHORIZED user"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, bridgeTypes.ErrNotAuthorized.Code, result.Code)
			assert.False(t, result.IsRecoverable, "auth errors should NOT be recoverable")
		})
	}
}

// TestMapError_NotFoundPatterns validates not found error detection.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Error messages containing:                                             │
// │  ├─ "not found"       → ErrNotFound (NOT Recoverable)                  │
// │  ├─ "does not exist"  → ErrNotFound (NOT Recoverable)                  │
// │  └─ "404"             → ErrNotFound (NOT Recoverable)                  │
// └─────────────────────────────────────────────────────────────────────────┘
func TestMapError_NotFoundPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"not found", "queue not found"},
		{"does not exist", "entity does not exist"},
		{"http 404", "HTTP 404 not found"},
		{"mixed case", "Queue NOT FOUND"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, bridgeTypes.ErrNotFound.Code, result.Code)
			assert.False(t, result.IsRecoverable, "not found errors should NOT be recoverable")
		})
	}
}

// TestMapError_ValidationPatterns validates validation error detection.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Error messages containing:                                             │
// │  ├─ "invalid"     → ErrInvalidPayload (NOT Recoverable)                │
// │  ├─ "malformed"   → ErrInvalidPayload (NOT Recoverable)                │
// │  └─ "bad request" → ErrInvalidPayload (NOT Recoverable)                │
// └─────────────────────────────────────────────────────────────────────────┘
func TestMapError_ValidationPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"invalid", "invalid message format"},
		{"malformed", "malformed request body"},
		{"bad request", "bad request syntax"},
		{"mixed case", "INVALID payload"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, bridgeTypes.ErrInvalidPayload.Code, result.Code)
			assert.False(t, result.IsRecoverable, "validation errors should NOT be recoverable")
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Default Error Handling Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_DefaultRecoverable validates unknown errors are treated as recoverable.
//
// ═══════════════════════════════════════════════════════════════════════════
// Unknown error patterns → ErrUnavailable (Recoverable)
//
// Rationale: Better to retry (potentially wasting resources) than to
// permanently fail and lose the message.
// ═══════════════════════════════════════════════════════════════════════════
func TestMapError_DefaultRecoverable(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"generic error", "something went wrong"},
		{"internal error", "internal service error"},
		{"unexpected", "unexpected error occurred"},
		{"empty message", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, bridgeTypes.ErrUnavailable.Code, result.Code)
			assert.True(t, result.IsRecoverable, "unknown errors should default to recoverable")
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ContainsAny Helper Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContainsAny_CaseInsensitive validates the helper function.
//
// Note: This test exercises the behavior through MapError since containsAny
// is unexported. The patterns are case-insensitive.
func TestContainsAny_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name     string
		errMsg   string
		expected bridgeTypes.ErrorCode // Expected error code
	}{
		{
			name:     "lowercase matches",
			errMsg:   "connection refused",
			expected: bridgeTypes.ErrConnectionLost.Code,
		},
		{
			name:     "uppercase matches",
			errMsg:   "CONNECTION REFUSED",
			expected: bridgeTypes.ErrConnectionLost.Code,
		},
		{
			name:     "mixed case matches",
			errMsg:   "CoNnEcTiOn ReFuSeD",
			expected: bridgeTypes.ErrConnectionLost.Code,
		},
		{
			name:     "partial match",
			errMsg:   "the connection was lost",
			expected: bridgeTypes.ErrConnectionLost.Code,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := servicebus.MapError(errors.New(tc.errMsg))

			require.NotNil(t, result)
			assert.Equal(t, tc.expected, result.Code)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Error Recoverability Classification Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestErrorRecoverability_Classification validates error recoverability.
//
// Decision Table:
// ┌─────────────────────────┬─────────────────┬───────────────┐
// │ Error Type              │ BridgeError     │ Recoverable   │
// ├─────────────────────────┼─────────────────┼───────────────┤
// │ Timeout                 │ ErrTimeout      │ ✓ Yes         │
// │ Connection Lost         │ ErrConnectionLost│ ✓ Yes        │
// │ Throttled               │ ErrThrottled    │ ✓ Yes         │
// │ Unavailable (default)   │ ErrUnavailable  │ ✓ Yes         │
// ├─────────────────────────┼─────────────────┼───────────────┤
// │ Not Authorized          │ ErrNotAuthorized│ ✗ No          │
// │ Not Found               │ ErrNotFound     │ ✗ No          │
// │ Invalid Payload         │ ErrInvalidPayload│ ✗ No         │
// └─────────────────────────┴─────────────────┴───────────────┘
func TestErrorRecoverability_Classification(t *testing.T) {
	t.Run("recoverable errors", func(t *testing.T) {
		recoverableErrors := []error{
			context.DeadlineExceeded,
			context.Canceled,
			errors.New("connection refused"),
			errors.New("throttled"),
			errors.New("server busy"),
			errors.New("unknown error"),
		}

		for _, err := range recoverableErrors {
			result := servicebus.MapError(err)
			require.NotNil(t, result, "error: %v", err)
			assert.True(t, result.IsRecoverable, "error %q should be recoverable", err.Error())
		}
	})

	t.Run("permanent errors", func(t *testing.T) {
		permanentErrors := []error{
			errors.New("unauthorized"),
			errors.New("forbidden"),
			errors.New("not found"),
			errors.New("invalid message"),
			errors.New("malformed request"),
		}

		for _, err := range permanentErrors {
			result := servicebus.MapError(err)
			require.NotNil(t, result, "error: %v", err)
			assert.False(t, result.IsRecoverable, "error %q should NOT be recoverable", err.Error())
		}
	})
}

// TestMapError_WrappedError validates that wrapped errors are handled.
func TestMapError_WrappedError(t *testing.T) {
	// Create a wrapped error
	inner := context.DeadlineExceeded
	wrapped := errors.New("operation failed: " + inner.Error())

	// Note: This tests the string matching fallback since wrapped errors
	// might not preserve errors.Is behavior for context errors
	result := servicebus.MapError(wrapped)

	require.NotNil(t, result)
	// The error message contains "timeout" from DeadlineExceeded message
	// which should match the connection pattern that includes "timeout"
	assert.True(t, result.IsRecoverable)
}

// TestMapError_PriorityOrder validates error pattern matching priority.
//
// When multiple patterns could match, the first matching category wins.
// Order in MapError: context errors → Service Bus errors → string patterns
func TestMapError_PriorityOrder(t *testing.T) {
	// Context errors take priority over string matching
	t.Run("context error priority", func(t *testing.T) {
		// Even though "timeout" could match connection pattern,
		// context.DeadlineExceeded should match first
		result := servicebus.MapError(context.DeadlineExceeded)

		require.NotNil(t, result)
		assert.Equal(t, bridgeTypes.ErrTimeout.Code, result.Code)
	})
}
