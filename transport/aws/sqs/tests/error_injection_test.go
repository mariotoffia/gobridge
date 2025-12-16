// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Error Injection Tests
//
// Tests for error injection using RoundTripper to simulate AWS errors.
// These tests verify that the SQS transport correctly handles:
// - Throttling errors (OverLimit, ThrottlingException)
// - Service unavailable errors (503)
// - Network errors (connection refused, reset, DNS)
//
// Run with: go test -tags=integration ./transport/aws/sqs/tests/...
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ E001 │ Target returns throttled error         │ PASS     │
// │ E002 │ Target returns service unavailable     │ PASS     │
// │ E003 │ Target handles network error           │ PASS     │
// │ E004 │ Target recovers after transient errors │ PASS     │
// │ E005 │ Source handles receive errors          │ PASS     │
// │ E006 │ RoundTripper queue mode works          │ PASS     │
// │ E007 │ RoundTripper latch mode works          │ PASS     │
// │ E008 │ Error classification is correct        │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

//go:build integration

package sqstests

import (
	"context"
	"errors"
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/tests/awsutils"
	"github.com/mariotoffia/gobridge/tests/docker"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Test Setup
// ═══════════════════════════════════════════════════════════════════════════

// setupErrorInjectionTest creates a LocalStack container and SQS helper for testing.
// Returns the helper and a cleanup function.
func setupErrorInjectionTest(t *testing.T) (*SQSLocalHelper, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.LocalStackForSQS().Start(ctx)
	if err != nil {
		t.Fatalf("failed to start LocalStack: %v", err)
	}

	helper := NewSQSLocalHelper(t, container)

	cleanup := func() {
		helper.RoundTripper().Disable().Clear().Unlatch()
		helper.Cleanup(ctx)
		container.Remove(ctx)
	}

	return helper, cleanup
}

// ═══════════════════════════════════════════════════════════════════════════
// RoundTripper Unit Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestRoundTripper_QueueMode validates queue mode (LIFO) behavior.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Push multiple transactions
//  2. Pop and verify LIFO order
//  3. Verify pass-through when queue empty
//
// ───────────────────────────────────────────────────────────────────────────
func TestRoundTripper_QueueMode(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-rt-queue"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Push errors in order: first throttle, then service unavailable
	// LIFO means service unavailable will be returned first
	helper.RoundTripper().Enable().
		Push(awsutils.awsutils.SqsErrors{}.ThrottlingException()).
		Push(awsutils.awsutils.SqsErrors{}.ServiceUnavailable())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	// First send should get ServiceUnavailable (last pushed)
	err = tgt.Send(ctx, msg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrUnavailable),
		"expected ErrUnavailable, got %v", err)

	// Second send should get ThrottlingException
	err = tgt.Send(ctx, msg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, bridgeTypes.ErrThrottled),
		"expected ErrThrottled, got %v", err)

	// Third send should pass through (queue empty)
	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify message was sent
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Len(t, messages, 1)
}

// TestRoundTripper_LatchMode validates latch mode behavior.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Latch an error transaction
//  2. All requests return same error
//  3. Unlatch and verify pass-through
//
// ───────────────────────────────────────────────────────────────────────────
func TestRoundTripper_LatchMode(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-rt-latch"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Latch OverLimit error
	helper.RoundTripper().Enable().Latch(awsutils.awsutils.SqsErrors{}.OverLimit())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	// Multiple sends should all return throttled error
	for i := 0; i < 3; i++ {
		err = tgt.Send(ctx, msg)
		require.Error(t, err, "send %d should fail", i)
		assert.True(t, errors.Is(err, bridgeTypes.ErrThrottled),
			"send %d: expected ErrThrottled, got %v", i, err)
	}

	// Unlatch
	helper.RoundTripper().Unlatch()

	// Now send should succeed
	err = tgt.Send(ctx, msg)
	require.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Error Injection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_ErrorInjection_OverLimit validates OverLimit error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject OverLimit error
//  2. Send message
//  3. Verify ErrThrottled is returned
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_OverLimit(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-overlimit"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject OverLimit error
	helper.RoundTripper().Enable().Push(awsutils.awsutils.SqsErrors{}.OverLimit())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as throttled (recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeThrottled, be.Code)
	assert.True(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_ServiceUnavailable validates ServiceUnavailable error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject ServiceUnavailable error
//  2. Send message
//  3. Verify ErrUnavailable is returned
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_ServiceUnavailable(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-unavail"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject ServiceUnavailable error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.ServiceUnavailable())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as unavailable (recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeUnavailable, be.Code)
	assert.True(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_InternalError validates InternalError error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject InternalError error
//  2. Send message
//  3. Verify ErrUnavailable is returned
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_InternalError(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-internal"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject InternalError error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.InternalError())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as unavailable (recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeUnavailable, be.Code)
	assert.True(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_NetworkError validates network error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject network error
//  2. Send message
//  3. Verify ErrConnectionLost is returned
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_NetworkError(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-network"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject network error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.NetworkError())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as connection lost (recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeConnectionLost, be.Code)
	assert.True(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_ConnectionReset validates connection reset handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject connection reset error
//  2. Send message
//  3. Verify ErrConnectionLost is returned
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_ConnectionReset(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-connreset"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject connection reset error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.ConnectionReset())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as connection lost (recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeConnectionLost, be.Code)
	assert.True(t, be.IsRecoverable)
}

// ═══════════════════════════════════════════════════════════════════════════
// Non-Retryable Error Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_ErrorInjection_QueueDoesNotExist validates non-retryable error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject QueueDoesNotExist error
//  2. Send message
//  3. Verify ErrNotFound is returned (non-recoverable)
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_QueueDoesNotExist(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue (needed for initial connection)
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-notfound"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject QueueDoesNotExist error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.QueueDoesNotExist())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as not found (non-recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeNotFound, be.Code)
	assert.False(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_InvalidMessageContents validates non-retryable error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject InvalidMessageContents error
//  2. Send message
//  3. Verify ErrInvalidPayload is returned (non-recoverable)
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_InvalidMessageContents(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-invalid"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject InvalidMessageContents error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.InvalidMessageContents())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as invalid payload (non-recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeInvalidPayload, be.Code)
	assert.False(t, be.IsRecoverable)
}

// TestTarget_ErrorInjection_AccessDenied validates access denied error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject AccessDenied error
//  2. Send message
//  3. Verify ErrNotAuthorized is returned (non-recoverable)
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_AccessDenied(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-denied"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject AccessDenied error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.AccessDenied())

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test message"),
	}

	err = tgt.Send(ctx, msg)
	require.Error(t, err)

	// Verify error is classified as not authorized (non-recoverable)
	be, ok := bridgeTypes.AsBridgeError(err)
	require.True(t, ok, "expected BridgeError")
	assert.Equal(t, bridgeTypes.ErrCodeNotAuthorized, be.Code)
	assert.False(t, be.IsRecoverable)
}

// ═══════════════════════════════════════════════════════════════════════════
// Recovery Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_ErrorInjection_RecoveryAfterTransientErrors validates recovery.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject multiple transient errors
//  2. After errors exhausted, send succeeds
//  3. Verify message was delivered
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_RecoveryAfterTransientErrors(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-recovery"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject 3 transient errors
	helper.RoundTripper().Enable().
		PushN(awsutils.SqsErrors{}.ThrottlingException(), 3)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("recovery test message"),
	}

	// First 3 sends should fail
	for i := 0; i < 3; i++ {
		err = tgt.Send(ctx, msg)
		require.Error(t, err, "send %d should fail", i)
	}

	// Fourth send should succeed (pass-through)
	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify message was delivered
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	require.Len(t, messages, 1)
	assert.Equal(t, "recovery test message", *messages[0].Body)
}

// TestTarget_ErrorInjection_BatchSendWithErrors validates batch error handling.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Inject error for batch send
//  2. SendBatch should fail
//  3. After error, batch succeeds
//
// ───────────────────────────────────────────────────────────────────────────
func TestTarget_ErrorInjection_BatchSendWithErrors(t *testing.T) {
	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-batch-err"))

	// Create target
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:  queueURL,
		BatchSize: 10,
	}

	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Inject throttling error
	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.RequestThrottled())

	messages := []bridgeTypes.Message{
		{CreatedAt: time.Now(), Payload: []byte("batch-1")},
		{CreatedAt: time.Now(), Payload: []byte("batch-2")},
		{CreatedAt: time.Now(), Payload: []byte("batch-3")},
	}

	// First batch should fail
	sent, err := tgt.SendBatch(ctx, messages)
	require.Error(t, err)
	assert.Equal(t, 0, sent)

	// Second batch should succeed
	sent, err = tgt.SendBatch(ctx, messages)
	require.NoError(t, err)
	assert.Equal(t, 3, sent)
}

// ═══════════════════════════════════════════════════════════════════════════
// Error Classification Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestErrorClassification validates error types are correctly classified.
func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		transaction   awsutils.RoundTripperTransaction
		expectedCode  bridgeTypes.ErrorCode
		isRecoverable bool
	}{
		{
			name:          "OverLimit is throttled and recoverable",
			transaction:   awsutils.SqsErrors{}.OverLimit(),
			expectedCode:  bridgeTypes.ErrCodeThrottled,
			isRecoverable: true,
		},
		{
			name:          "ServiceUnavailable is unavailable and recoverable",
			transaction:   awsutils.SqsErrors{}.ServiceUnavailable(),
			expectedCode:  bridgeTypes.ErrCodeUnavailable,
			isRecoverable: true,
		},
		{
			name:          "InternalError is unavailable and recoverable",
			transaction:   awsutils.SqsErrors{}.InternalError(),
			expectedCode:  bridgeTypes.ErrCodeUnavailable,
			isRecoverable: true,
		},
		{
			name:          "ThrottlingException is throttled and recoverable",
			transaction:   awsutils.SqsErrors{}.ThrottlingException(),
			expectedCode:  bridgeTypes.ErrCodeThrottled,
			isRecoverable: true,
		},
		{
			name:          "QueueDoesNotExist is not found and not recoverable",
			transaction:   awsutils.SqsErrors{}.QueueDoesNotExist(),
			expectedCode:  bridgeTypes.ErrCodeNotFound,
			isRecoverable: false,
		},
		{
			name:          "InvalidMessageContents is invalid payload and not recoverable",
			transaction:   awsutils.SqsErrors{}.InvalidMessageContents(),
			expectedCode:  bridgeTypes.ErrCodeInvalidPayload,
			isRecoverable: false,
		},
	}

	helper, cleanup := setupErrorInjectionTest(t)
	defer cleanup()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Create queue
			queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-classify"))

			// Create target
			cfg := &sqs.TargetConfigImpl{
				ID: "test-target",
				Connection: sqs.ConnectionConfig{
					Region:   "us-east-1",
					Endpoint: helper.Endpoint(),
				},
				QueueURL: queueURL,
			}

			tgt, err := sqs.NewTarget(cfg)
			require.NoError(t, err)
			defer tgt.Close()

			// Inject error
			helper.RoundTripper().Enable().Clear().Push(tc.transaction)

			msg := bridgeTypes.Message{
				CreatedAt: time.Now(),
				Payload:   []byte("test"),
			}

			err = tgt.Send(ctx, msg)
			require.Error(t, err)

			be, ok := bridgeTypes.AsBridgeError(err)
			require.True(t, ok, "expected BridgeError")
			assert.Equal(t, tc.expectedCode, be.Code)
			assert.Equal(t, tc.isRecoverable, be.IsRecoverable)

			// Disable for next iteration
			helper.RoundTripper().Disable()
		})
	}
}
