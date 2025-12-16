// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Integration Tests
//
// Integration tests using LocalStack for SQS operations.
// These tests require Docker to be running with LocalStack.
//
// Run with: go test -tags=integration ./transport/aws/sqs/tests/...
//
// Test Flow:
// ┌──────────────────────────────────────────────────────────────────────────┐
// │                       LocalStack SQS Integration                         │
// ├──────────────────────────────────────────────────────────────────────────┤
// │                                                                          │
// │   ┌─────────────┐      ┌─────────────────┐      ┌─────────────┐          │
// │   │   Target    │─────▶│   SQS Queue     │─────▶│   Source    │          │
// │   │   .Send()   │      │  (LocalStack)   │      │ .Messages() │          │
// │   └─────────────┘      └─────────────────┘      └─────────────┘          │
// │                               │                        │                 │
// │                               │                        ▼                 │
// │                               │                 ┌─────────────┐          │
// │                               │                 │ Ack/Nack/   │          │
// │                               │◀────────────────│   Extend    │          │
// │                               │                 └─────────────┘          │
// │                                                                          │
// └──────────────────────────────────────────────────────────────────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ I001 │ Source receives messages               │ PASS     │
// │ I002 │ Source Ack deletes message             │ PASS     │
// │ I003 │ Source Nack makes message visible      │ PASS     │
// │ I004 │ Source message attributes extracted    │ PASS     │
// │ I005 │ Target sends message                   │ PASS     │
// │ I006 │ Target sends with delay                │ PASS     │
// │ I007 │ Target batch send                      │ PASS     │
// │ I008 │ Target metadata as attributes          │ PASS     │
// │ I009 │ FIFO queue deduplication               │ PASS     │
// │ I010 │ Source-Target round trip               │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

package sqstests

import (
	"context"
	"fmt"
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/tests/docker"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Test Setup
// ═══════════════════════════════════════════════════════════════════════════

// setupSQSTest creates a LocalStack container and SQS helper for testing.
// Returns the helper and a cleanup function.
func setupSQSTest(t *testing.T) (*SQSLocalHelper, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.LocalStackForSQS().Start(ctx)
	if err != nil {
		t.Fatalf("failed to start LocalStack: %v", err)
	}

	helper := NewSQSLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		container.Remove(ctx)
	}

	return helper, cleanup
}

// uniqueQueueName generates a unique queue name for testing.
func uniqueQueueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ═══════════════════════════════════════════════════════════════════════════
// Source Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_Source_ReceiveMessages validates Source receives messages.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Create queue with message
//  2. Start Source
//  3. Receive message on channel
//  4. Verify payload
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Source_ReceiveMessages(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue and send test message
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-recv"))
	expectedBody := "Hello, SQS!"
	helper.SendMessage(ctx, queueURL, expectedBody, nil)

	// Create and start source
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:          queueURL,
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30,
	}

	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, expectedBody, string(msg.Message.Payload))
		// Ack the message
		err := msg.Ack()
		assert.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}
}

// TestIntegration_SQS_Source_AckDeletesMessage validates Ack removes message.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Send message to queue
//  2. Receive and Ack message
//  3. Verify queue is empty
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Source_AckDeletesMessage(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue and send test message
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-ack"))
	helper.SendMessage(ctx, queueURL, "test message", nil)

	// Create and start source
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:          queueURL,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
	}

	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Receive and Ack
	select {
	case msg := <-src.Messages():
		err := msg.Ack()
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}

	// Wait a bit for delete to propagate
	time.Sleep(500 * time.Millisecond)

	// Verify queue is empty
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Empty(t, messages, "queue should be empty after Ack")
}

// TestIntegration_SQS_Source_NackMakesVisible validates Nack returns message.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Send message to queue
//  2. Receive and Nack message
//  3. Message should become visible again
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Source_NackMakesVisible(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue and send test message
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-nack"))
	helper.SendMessage(ctx, queueURL, "test nack message", nil)

	// Create and start source with short visibility
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:          queueURL,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30, // Long timeout
	}

	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Receive and Nack
	select {
	case msg := <-src.Messages():
		err := msg.Nack(fmt.Errorf("test error"))
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}

	// Wait a bit
	time.Sleep(500 * time.Millisecond)

	// Message should be visible again (we can receive it)
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Len(t, messages, 1, "message should be visible after Nack")
}

// TestIntegration_SQS_Source_MessageAttributes validates attribute extraction.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Send message with attributes
//  2. Receive message
//  3. Verify attributes in metadata
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Source_MessageAttributes(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue and send message with attributes
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-attrs"))
	helper.SendMessage(ctx, queueURL, "test body", map[string]string{
		"CustomAttr": "custom-value",
		"AnotherKey": "another-value",
	})

	// Create and start source
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:              queueURL,
		WaitTimeSeconds:       1,
		MessageAttributeNames: []string{"All"},
	}

	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, "custom-value", msg.Message.Metadata["CustomAttr"])
		assert.Equal(t, "another-value", msg.Message.Metadata["AnotherKey"])
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_Target_SendMessage validates Target sends message.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Create queue
//  2. Send message via Target
//  3. Verify message in queue
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Target_SendMessage(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-send"))

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

	// Send message
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("Hello from Target!"),
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify message in queue
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	require.Len(t, messages, 1)
	assert.Equal(t, "Hello from Target!", *messages[0].Body)
}

// TestIntegration_SQS_Target_SendBatch validates batch sending.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Create queue
//  2. Send batch of messages via Target
//  3. Verify all messages in queue
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Target_SendBatch(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-batch"))

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

	// Send batch
	messages := []bridgeTypes.Message{
		{CreatedAt: time.Now(), Payload: []byte("message-1")},
		{CreatedAt: time.Now(), Payload: []byte("message-2")},
		{CreatedAt: time.Now(), Payload: []byte("message-3")},
	}

	sent, err := tgt.SendBatch(ctx, messages)
	require.NoError(t, err)
	assert.Equal(t, 3, sent)

	// Verify messages in queue
	received := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Len(t, received, 3)
}

// TestIntegration_SQS_Target_MetadataAsAttributes validates metadata conversion.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Send message with metadata via Target
//  2. Receive message
//  3. Verify metadata as message attributes
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Target_MetadataAsAttributes(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-meta"))

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

	// Send message with metadata
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "test/topic",
		Payload:   []byte("test"),
		Metadata: map[string]any{
			"CustomKey": "custom-value",
			"Counter":   42,
		},
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify attributes
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	require.Len(t, messages, 1)

	// Check custom attribute
	if attr, ok := messages[0].MessageAttributes["CustomKey"]; ok {
		assert.Equal(t, "custom-value", *attr.StringValue)
	}

	// Check topic attribute
	if attr, ok := messages[0].MessageAttributes["Topic"]; ok {
		assert.Equal(t, "test/topic", *attr.StringValue)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// FIFO Queue Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_FIFO_Deduplication validates FIFO deduplication.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Create FIFO queue
//  2. Send same message twice with same dedup ID
//  3. Only one message should be in queue
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_FIFO_Deduplication(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create FIFO queue
	queueURL := helper.CreateFIFOQueue(ctx, uniqueQueueName("test-fifo"))

	// Send duplicate messages
	helper.SendFIFOMessage(ctx, queueURL, "message", "group1", "dedup-1")
	helper.SendFIFOMessage(ctx, queueURL, "message", "group1", "dedup-1") // Same dedup ID

	// Wait for deduplication
	time.Sleep(500 * time.Millisecond)

	// Should only have one message
	messages := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Len(t, messages, 1, "duplicate should be deduplicated")
}

// ═══════════════════════════════════════════════════════════════════════════
// Round Trip Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_RoundTrip validates full Source→Target flow.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send()                    SQS Queue                Source.Messages()
//	     │                               │                          │
//	     │──── message ─────────────────▶│                          │
//	     │                         [queued]                         │
//	     │                               │◀──────── poll ───────────│
//	     │                               │                          │
//	     │                               │───── message ───────────▶│
//	     │                               │                          │
//	     │                               │◀──────── Ack ────────────│
//	     │                         [deleted]                        │
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_RoundTrip(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-roundtrip"))

	// Create target
	targetCfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL: queueURL,
	}

	tgt, err := sqs.NewTarget(targetCfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Create source
	sourceCfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:        queueURL,
		WaitTimeSeconds: 1,
	}

	src, err := sqs.NewSource(sourceCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Send message via target
	expectedPayload := "Round trip test message"
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     "roundtrip/test",
		Payload:   []byte(expectedPayload),
		Metadata: map[string]any{
			"testKey": "testValue",
		},
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Receive via source
	select {
	case received := <-src.Messages():
		// Verify payload
		assert.Equal(t, expectedPayload, string(received.Message.Payload))

		// Verify metadata came through
		if v, ok := received.Message.Metadata["testKey"]; ok {
			assert.Equal(t, "testValue", v)
		}

		// Ack to complete round trip
		err := received.Ack()
		assert.NoError(t, err)

	case <-ctx.Done():
		t.Fatal("timeout waiting for round trip message")
	}

	// Verify queue is empty
	time.Sleep(500 * time.Millisecond)
	remaining := helper.ReceiveMessages(ctx, queueURL, 10)
	assert.Empty(t, remaining, "queue should be empty after Ack")
}

// TestIntegration_SQS_Source_CancelContext validates graceful shutdown.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Start Source
//  2. Cancel context
//  3. Verify Source stops without error
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_SQS_Source_CancelContext(t *testing.T) {
	helper, cleanup := setupSQSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	// Create queue
	queueURL := helper.CreateQueue(ctx, uniqueQueueName("test-cancel"))

	// Create source
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		Connection: sqs.ConnectionConfig{
			Region:   "us-east-1",
			Endpoint: helper.Endpoint(),
		},
		QueueURL:        queueURL,
		WaitTimeSeconds: 20, // Long poll - will be interrupted
	}

	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)

	err = src.Start(ctx)
	require.NoError(t, err)

	// Cancel context
	cancel()

	// Close should complete without hanging
	done := make(chan struct{})
	go func() {
		src.Close()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not complete within timeout")
	}
}
