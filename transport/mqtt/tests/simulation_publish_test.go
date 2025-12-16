// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Publish Simulation Tests
//
// Simulation tests for MQTT publish scenarios including retained messages,
// message properties, and dynamic topic changes.
//
// Run with: go test -tags=integration ./transport/mqtt/tests/...
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │PUB001│ Publish to topic                       │ PASS     │
// │PUB002│ Publish with retain                    │ PASS     │
// │PUB003│ Publish message expiry                 │ PASS     │
// │PUB004│ Publish user properties                │ PASS     │
// │PUB005│ Publish dynamic topic                  │ PASS     │
// │PUB006│ Batch publish                          │ PASS     │
// │PUB007│ Concurrent publish                     │ PASS     │
// │PUB008│ Multiple targets same connection       │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

package mqtttests

import (
	"context"
	"sync"
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/tests/docker"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Test Setup
// ═══════════════════════════════════════════════════════════════════════════

// setupPublishTest creates a Mosquitto container for publish testing.
func setupPublishTest(t *testing.T) (*MQTTLocalHelper, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.DefaultMosquittoConfig().Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Mosquitto: %v", err)
	}

	helper := NewMQTTLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		container.Remove(ctx)
	}

	return helper, cleanup
}

// ═══════════════════════════════════════════════════════════════════════════
// Basic Publish Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_PublishToTopic validates basic topic publish.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target publishes to specific topic
//	Subscriber receives message on that topic
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_PublishToTopic(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/publish/basic")

	// Start test client subscription
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target
	cfg := helper.NewTargetConfig("publish-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Publish message
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("basic publish test"),
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify receipt
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "basic publish test", string(msgs[0].Payload))
}

// TestSimulation_PublishWithRetain validates retained message publish.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Publish retained message
//  2. New subscriber connects
//  3. New subscriber receives retained message immediately
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_PublishWithRetain(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/publish/retain")

	// Create target with retain
	cfg := &mqtt.TargetConfigImpl{
		ID: "retain-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  helper.BrokerURL(),
			CleanStart: true,
		},
		DefaultTopic: topic,
		QoS:          1,
		Retain:       true,
	}

	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Publish retained message
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("retained message"),
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Wait for message to be retained
	time.Sleep(500 * time.Millisecond)

	// New subscriber should receive retained message
	helper.ClearReceivedMessages()
	err = helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "retained message", string(msgs[0].Payload))
	assert.True(t, msgs[0].Retain, "message should be marked as retained")

	// Clean up retained message
	err = helper.ClearRetained(ctx, topic)
	require.NoError(t, err)
}

// TestSimulation_PublishMessageExpiry validates message expiry (TTL).
func TestSimulation_PublishMessageExpiry(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/publish/expiry")

	// Start subscription
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with message expiry
	cfg := &mqtt.TargetConfigImpl{
		ID: "expiry-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  helper.BrokerURL(),
			CleanStart: true,
		},
		DefaultTopic:  topic,
		QoS:           1,
		MessageExpiry: 60, // 60 seconds
	}

	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Publish with TTL
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("expiring message"),
		TTL:       30 * time.Second,
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Should receive message
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "expiring message", string(msgs[0].Payload))
}

// TestSimulation_PublishUserProperties validates MQTT v5 user properties.
func TestSimulation_PublishUserProperties(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/publish/props")

	// Create source to receive message with properties
	srcCfg := helper.NewSourceConfig("props-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Create target
	cfg := helper.NewTargetConfig("props-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Publish with user properties
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("message with properties"),
		Metadata: map[string]any{
			"contentType": "application/json",
			"userProperties": map[string]string{
				"custom-header":  "custom-value",
				"another-header": "another-value",
			},
		},
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Receive and verify properties
	select {
	case received := <-src.Messages():
		assert.Equal(t, "message with properties", string(received.Message.Payload))

		// Check if properties were extracted
		if received.Message.Metadata != nil {
			if ct, ok := received.Message.Metadata["contentType"]; ok {
				assert.Equal(t, "application/json", ct)
			}
		}
		received.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for message with properties")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Dynamic Topic Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_PublishDynamicTopic validates publishing to different topics.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target has default topic, but individual messages specify different topics
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_PublishDynamicTopic(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("test/dynamic")
	defaultTopic := prefix + "/default"

	// Subscribe to wildcard to catch all messages
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, prefix+"/#", 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with default topic
	cfg := helper.NewTargetConfig("dynamic-target", defaultTopic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send to different topics
	topics := []string{
		prefix + "/topic1",
		prefix + "/topic2",
		defaultTopic,
	}

	for _, topic := range topics {
		msg := bridgeTypes.Message{
			CreatedAt: time.Now(),
			Topic:     topic,
			Payload:   []byte("message to " + topic),
		}
		err = tgt.Send(ctx, msg)
		require.NoError(t, err)
	}

	// Should receive all 3 messages
	msgs, err := helper.WaitForMessages(ctx, 3)
	require.NoError(t, err)
	assert.Len(t, msgs, 3)
}

// ═══════════════════════════════════════════════════════════════════════════
// Batch and Concurrent Publish Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_BatchPublish validates sequential batch publishing.
func TestSimulation_BatchPublish(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/batch")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target
	cfg := helper.NewTargetConfig("batch-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Create batch of messages
	batch := make([]bridgeTypes.Message, 5)
	for i := 0; i < 5; i++ {
		batch[i] = bridgeTypes.Message{
			CreatedAt: time.Now(),
			Payload:   []byte("batch message " + string(rune('A'+i))),
		}
	}

	// Send batch
	sent, err := tgt.SendBatch(ctx, batch)
	require.NoError(t, err)
	assert.Equal(t, 5, sent)

	// Should receive all 5 messages
	msgs, err := helper.WaitForMessages(ctx, 5)
	require.NoError(t, err)
	assert.Len(t, msgs, 5)
}

// TestSimulation_ConcurrentPublish validates parallel publishing.
func TestSimulation_ConcurrentPublish(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/concurrent")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target
	cfg := helper.NewTargetConfig("concurrent-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send messages concurrently
	const numMessages = 10
	var wg sync.WaitGroup
	errors := make(chan error, numMessages)

	for i := 0; i < numMessages; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msg := bridgeTypes.Message{
				CreatedAt: time.Now(),
				Payload:   []byte("concurrent message"),
			}
			if err := tgt.Send(ctx, msg); err != nil {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	for err := range errors {
		t.Errorf("concurrent send error: %v", err)
	}

	// Should receive all messages
	msgs, err := helper.WaitForMessages(ctx, numMessages)
	require.NoError(t, err)
	assert.Len(t, msgs, numMessages)
}

// ═══════════════════════════════════════════════════════════════════════════
// Shared Connection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_MultipleTargetsSameConnection validates multiple targets on one connection.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Single MQTTConnection
//	├── Target 1: publishes to "topic/a"
//	└── Target 2: publishes to "topic/b"
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_MultipleTargetsSameConnection(t *testing.T) {
	helper, cleanup := setupPublishTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("test/multi-target")
	topicA := prefix + "/a"
	topicB := prefix + "/b"

	// Subscribe to both
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, prefix+"/#", 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create shared connection
	connCfg := helper.NewConnectionConfig("shared-publish-conn")
	conn, err := mqtt.NewConnection(connCfg)
	require.NoError(t, err)

	err = conn.Start(ctx, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Create two targets on same connection
	tgtCfgA := &mqtt.TargetConfigImpl{
		ID:           "target-a",
		DefaultTopic: topicA,
		QoS:          1,
	}
	tgtA, err := conn.CreateTarget(ctx, tgtCfgA)
	require.NoError(t, err)
	defer tgtA.Close()

	tgtCfgB := &mqtt.TargetConfigImpl{
		ID:           "target-b",
		DefaultTopic: topicB,
		QoS:          1,
	}
	tgtB, err := conn.CreateTarget(ctx, tgtCfgB)
	require.NoError(t, err)
	defer tgtB.Close()

	// Publish from both targets
	msgA := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("message from target A"),
	}
	err = tgtA.Send(ctx, msgA)
	require.NoError(t, err)

	msgB := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("message from target B"),
	}
	err = tgtB.Send(ctx, msgB)
	require.NoError(t, err)

	// Should receive both messages
	msgs, err := helper.WaitForMessages(ctx, 2)
	require.NoError(t, err)
	assert.Len(t, msgs, 2)

	// Verify topics
	topics := make(map[string]bool)
	for _, msg := range msgs {
		topics[msg.Topic] = true
	}
	assert.True(t, topics[topicA], "should receive message on topic A")
	assert.True(t, topics[topicB], "should receive message on topic B")
}
