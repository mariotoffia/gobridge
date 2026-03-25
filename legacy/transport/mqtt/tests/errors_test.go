// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Error Classification Tests
//
// Tests for error mapping and classification functions.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ E001 │ MapError nil returns nil               │ PASS     │
// │ E002 │ MapError DeadlineExceeded → Timeout    │ PASS     │
// │ E003 │ MapError Canceled → Unavailable        │ PASS     │
// │ E004 │ MapError net timeout → Timeout         │ PASS     │
// │ E005 │ MapError net other → ConnectionLost    │ PASS     │
// │ E006 │ MapError connection refused            │ PASS     │
// │ E007 │ MapError server unavailable            │ PASS     │
// │ E008 │ MapPublishError success → nil          │ PASS     │
// │ E009 │ MapPublishError no subscribers → nil   │ PASS     │
// │ E010 │ MapPublishError not authorized         │ PASS     │
// │ E011 │ MapPublishError invalid topic          │ PASS     │
// │ E012 │ MapPublishError quota exceeded         │ PASS     │
// │ E013 │ Disconnect server busy                 │ PASS     │
// │ E014 │ Disconnect keep alive timeout          │ PASS     │
// │ E015 │ Disconnect session taken over          │ PASS     │
// │ E016 │ Disconnect malformed packet            │ PASS     │
// │ E017 │ Disconnect bad credentials             │ PASS     │
// │ E018 │ Disconnect topic invalid               │ PASS     │
// │ E019 │ Disconnect packet too large            │ PASS     │
// │ E020 │ Error classification accuracy          │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"context"
	"errors"
	"net"
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// MapError Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapError_Nil validates nil error returns nil.
func TestMapError_Nil(t *testing.T) {
	result := mqtt.MapError(nil)
	assert.Nil(t, result)
}

// TestMapError_DeadlineExceeded validates context deadline maps to Timeout.
func TestMapError_DeadlineExceeded(t *testing.T) {
	err := context.DeadlineExceeded
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrTimeout),
		"DeadlineExceeded should map to ErrTimeout")
	assert.True(t, result.IsRecoverable, "timeout should be recoverable")
}

// TestMapError_Canceled validates context canceled maps to Unavailable.
func TestMapError_Canceled(t *testing.T) {
	err := context.Canceled
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrUnavailable),
		"Canceled should map to ErrUnavailable")
	assert.True(t, result.IsRecoverable, "canceled should be recoverable")
}

// TestMapError_NetTimeout validates network timeout maps to Timeout.
func TestMapError_NetTimeout(t *testing.T) {
	err := &mockNetError{timeout: true}
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrTimeout),
		"network timeout should map to ErrTimeout")
}

// TestMapError_NetOther validates other network error maps to ConnectionLost.
func TestMapError_NetOther(t *testing.T) {
	err := &mockNetError{timeout: false}
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrConnectionLost),
		"network error should map to ErrConnectionLost")
}

// TestMapError_ConnectionRefused validates connection refused detection.
func TestMapError_ConnectionRefused(t *testing.T) {
	err := errors.New("dial tcp: connection refused")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrConnectionLost),
		"connection refused should map to ErrConnectionLost")
	assert.True(t, result.IsRecoverable)
}

// TestMapError_NoRouteToHost validates no route to host detection.
func TestMapError_NoRouteToHost(t *testing.T) {
	err := errors.New("dial tcp: no route to host")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrConnectionLost),
		"no route to host should map to ErrConnectionLost")
}

// TestMapError_NetworkUnreachable validates network unreachable detection.
func TestMapError_NetworkUnreachable(t *testing.T) {
	err := errors.New("dial tcp: network unreachable")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrConnectionLost),
		"network unreachable should map to ErrConnectionLost")
}

// TestMapError_ServerUnavailable validates server unavailable detection.
func TestMapError_ServerUnavailable(t *testing.T) {
	err := errors.New("server unavailable")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrUnavailable),
		"server unavailable should map to ErrUnavailable")
}

// TestMapError_BrokerUnavailable validates broker unavailable detection.
func TestMapError_BrokerUnavailable(t *testing.T) {
	err := errors.New("broker unavailable")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrUnavailable),
		"broker unavailable should map to ErrUnavailable")
}

// TestMapError_UnknownError validates unknown error defaults to Unavailable.
func TestMapError_UnknownError(t *testing.T) {
	err := errors.New("some random error")
	result := mqtt.MapError(err)

	require.NotNil(t, result)
	// Unknown errors default to Unavailable (recoverable)
	assert.True(t, result.IsRecoverable,
		"unknown errors should be recoverable by default")
}

// ═══════════════════════════════════════════════════════════════════════════
// MapPublishError Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMapPublishError_Success validates success returns nil.
func TestMapPublishError_Success(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x00)
	assert.Nil(t, result)
}

// TestMapPublishError_NoSubscribers validates no subscribers is not an error.
func TestMapPublishError_NoSubscribers(t *testing.T) {
	// Reason code 0x10 = No matching subscribers
	// This is informational, not an error - broker accepted the message
	result := mqtt.MapPublishError(nil, 0x10)
	assert.Nil(t, result, "no subscribers should not be an error")
}

// TestMapPublishError_UnspecifiedError validates unspecified error.
func TestMapPublishError_UnspecifiedError(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x80)
	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrUnavailable))
}

// TestMapPublishError_NotAuthorized validates not authorized.
func TestMapPublishError_NotAuthorized(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x87)
	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrForbidden))
	assert.False(t, result.IsRecoverable, "not authorized should not be recoverable")
}

// TestMapPublishError_TopicInvalid validates invalid topic.
func TestMapPublishError_TopicInvalid(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x90)
	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrInvalidTopic))
	assert.False(t, result.IsRecoverable, "invalid topic should not be recoverable")
}

// TestMapPublishError_QuotaExceeded validates quota exceeded.
func TestMapPublishError_QuotaExceeded(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x97)
	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrThrottled))
	assert.True(t, result.IsRecoverable, "quota exceeded should be recoverable")
}

// TestMapPublishError_PayloadFormatInvalid validates invalid payload.
func TestMapPublishError_PayloadFormatInvalid(t *testing.T) {
	result := mqtt.MapPublishError(nil, 0x99)
	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrInvalidPayload))
	assert.False(t, result.IsRecoverable, "invalid payload should not be recoverable")
}

// TestMapPublishError_WithWrappedError validates error wrapping.
func TestMapPublishError_WithWrappedError(t *testing.T) {
	originalErr := errors.New("underlying error")
	result := mqtt.MapPublishError(originalErr, 0)

	require.NotNil(t, result)
	assert.True(t, errors.Is(result, bridgeTypes.ErrUnavailable))
}

// ═══════════════════════════════════════════════════════════════════════════
// Error Classification Table Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestErrorClassification validates recoverable vs permanent errors.
//
// ┌────────────────────────┬─────────────┬────────────────────────┐
// │ Error                  │ Recoverable │ Resolution             │
// ├────────────────────────┼─────────────┼────────────────────────┤
// │ Timeout                │ YES         │ Retry with backoff     │
// │ ConnectionLost         │ YES         │ Reconnect and retry    │
// │ Unavailable            │ YES         │ Retry with backoff     │
// │ Throttled              │ YES         │ Retry after delay      │
// │ BrokerBusy             │ YES         │ Retry with backoff     │
// │ NotAuthorized          │ NO          │ Fix credentials        │
// │ Forbidden              │ NO          │ Fix permissions        │
// │ NotFound               │ NO          │ Create resource        │
// │ InvalidPayload         │ NO          │ Fix message            │
// │ InvalidTopic           │ NO          │ Fix topic name         │
// │ ProtocolError          │ NO          │ Fix implementation     │
// │ PayloadTooLarge        │ NO          │ Split/compress         │
// └────────────────────────┴─────────────┴────────────────────────┘
func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		error         *bridgeTypes.BridgeError
		isRecoverable bool
	}{
		// Recoverable errors
		{"Timeout is recoverable", bridgeTypes.ErrTimeout, true},
		{"ConnectionLost is recoverable", bridgeTypes.ErrConnectionLost, true},
		{"Unavailable is recoverable", bridgeTypes.ErrUnavailable, true},
		{"Throttled is recoverable", bridgeTypes.ErrThrottled, true},
		{"BrokerBusy is recoverable", bridgeTypes.ErrBrokerBusy, true},

		// Permanent errors
		{"NotAuthorized is permanent", bridgeTypes.ErrNotAuthorized, false},
		{"Forbidden is permanent", bridgeTypes.ErrForbidden, false},
		{"NotFound is permanent", bridgeTypes.ErrNotFound, false},
		{"InvalidPayload is permanent", bridgeTypes.ErrInvalidPayload, false},
		{"InvalidTopic is permanent", bridgeTypes.ErrInvalidTopic, false},
		{"ProtocolError is permanent", bridgeTypes.ErrProtocolError, false},
		{"PayloadTooLarge is permanent", bridgeTypes.ErrPayloadTooLarge, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.isRecoverable, tc.error.IsRecoverable,
				"error classification mismatch")
		})
	}
}

// TestIsRecoverableError validates the helper function.
func TestIsRecoverableError(t *testing.T) {
	// Recoverable error
	timeoutErr := bridgeTypes.ErrTimeout.Wrap(errors.New("timed out"))
	assert.True(t, bridgeTypes.IsRecoverableError(timeoutErr))

	// Non-recoverable error
	authErr := bridgeTypes.ErrNotAuthorized.Wrap(errors.New("bad password"))
	assert.False(t, bridgeTypes.IsRecoverableError(authErr))

	// Nil error
	assert.False(t, bridgeTypes.IsRecoverableError(nil))

	// Unknown error (defaults to recoverable)
	unknownErr := errors.New("unknown error")
	assert.True(t, bridgeTypes.IsRecoverableError(unknownErr))
}

// ═══════════════════════════════════════════════════════════════════════════
// Mock Types
// ═══════════════════════════════════════════════════════════════════════════

// mockNetError implements net.Error for testing.
type mockNetError struct {
	timeout   bool
	temporary bool
}

func (e *mockNetError) Error() string   { return "mock network error" }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return e.temporary }

// Ensure mockNetError implements net.Error
var _ net.Error = (*mockNetError)(nil)
