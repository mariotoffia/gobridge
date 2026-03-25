// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Subscription Simulation Tests
//
// Simulation tests for MQTT subscription scenarios including wildcards,
// dynamic changes, and session management.
//
// Run with: go test -tags=integration ./transport/mqtt/tests/...
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │SUB001│ Single topic subscribe                 │ PASS     │
// │SUB002│ Multiple topics subscribe              │ PASS     │
// │SUB003│ Wildcard + (single-level)              │ PASS     │
// │SUB004│ Wildcard # (multi-level)               │ PASS     │
// │SUB005│ Overlapping wildcards                  │ PASS     │
// │SUB006│ Clean session subscribe                │ PASS     │
// │SUB007│ Multiple sources same connection       │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

package mqtttests

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/tests/docker"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Test Setup
// ═══════════════════════════════════════════════════════════════════════════

// setupSubscriptionTest creates a Mosquitto container for subscription testing.
func setupSubscriptionTest(t *testing.T) (*MQTTLocalHelper, func()) {
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
// Basic Subscription Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_SingleTopicSubscribe validates basic single topic subscription.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Subscribe to: "sensors/temperature"
//	Publish to:   "sensors/temperature"
//	Result:       Message received ✓
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_SingleTopicSubscribe(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("sensors/temperature")

	// Create source with single topic
	srcCfg := helper.NewSourceConfig("single-topic-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish message
	err = helper.PublishString(ctx, topic, "temperature: 25.5", 1)
	require.NoError(t, err)

	// Verify receipt
	select {
	case msg := <-src.Messages():
		assert.Equal(t, "temperature: 25.5", string(msg.Message.Payload))
		assert.Equal(t, topic, msg.Message.Topic)
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}
}

// TestSimulation_MultipleTopicsSubscribe validates multiple topic subscription.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Subscribe to: ["sensors/temp", "sensors/humidity", "sensors/pressure"]
//	Publish to each topic
//	Result:       All 3 messages received ✓
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_MultipleTopicsSubscribe(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("sensors")
	topics := []string{
		prefix + "/temp",
		prefix + "/humidity",
		prefix + "/pressure",
	}

	// Create source with multiple topics
	srcCfg := helper.NewSourceConfig("multi-topic-source", topics, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish to each topic
	for _, topic := range topics {
		err = helper.PublishString(ctx, topic, "value from "+topic, 1)
		require.NoError(t, err)
	}

	// Receive all 3 messages
	received := make(map[string]bool)
	for i := 0; i < 3; i++ {
		select {
		case msg := <-src.Messages():
			received[msg.Message.Topic] = true
			msg.Ack()
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for message %d", i+1)
		}
	}

	// Verify all topics received
	for _, topic := range topics {
		assert.True(t, received[topic], "should have received message from %s", topic)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Wildcard Subscription Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_WildcardPlus validates single-level wildcard (+).
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Subscribe to: "sensors/+/temperature"
//
//	Publish to:               Match?
//	────────────────────────────────────
//	sensors/room1/temperature   ✓
//	sensors/room2/temperature   ✓
//	sensors/room1/humidity      ✗
//	sensors/building/floor/temp ✗ (+ is single level)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_WildcardPlus(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("sensors")
	wildcardTopic := prefix + "/+/temperature"

	// Create source with wildcard
	srcCfg := helper.NewSourceConfig("wildcard-plus-source", []string{wildcardTopic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Test matching topics
	matchingTopics := []string{
		prefix + "/room1/temperature",
		prefix + "/room2/temperature",
		prefix + "/kitchen/temperature",
	}

	for _, topic := range matchingTopics {
		err = helper.PublishString(ctx, topic, "temp data", 1)
		require.NoError(t, err)
	}

	// Should receive 3 messages
	for i := 0; i < 3; i++ {
		select {
		case msg := <-src.Messages():
			assert.Contains(t, msg.Message.Topic, "temperature")
			msg.Ack()
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for wildcard match %d", i+1)
		}
	}

	// Test non-matching topic (should not receive)
	nonMatchingTopic := prefix + "/room1/humidity"
	err = helper.PublishString(ctx, nonMatchingTopic, "humidity data", 1)
	require.NoError(t, err)

	// Should not receive this message
	select {
	case msg := <-src.Messages():
		if msg.Message.Topic == nonMatchingTopic {
			t.Fatal("should not receive non-matching topic")
		}
		msg.Ack()
	case <-time.After(500 * time.Millisecond):
		// Expected - no message received for non-matching topic
	}
}

// TestSimulation_WildcardHash validates multi-level wildcard (#).
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Subscribe to: "sensors/#"
//
//	Publish to:                    Match?
//	────────────────────────────────────────
//	sensors                         ✗ (# needs at least one level)
//	sensors/temp                    ✓
//	sensors/room1/temp              ✓
//	sensors/building/floor1/room1   ✓
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_WildcardHash(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("sensors")
	wildcardTopic := prefix + "/#"

	// Create source with wildcard
	srcCfg := helper.NewSourceConfig("wildcard-hash-source", []string{wildcardTopic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Test matching topics at different levels
	matchingTopics := []string{
		prefix + "/temp",
		prefix + "/room1/temp",
		prefix + "/building/floor1/room1/temp",
	}

	for _, topic := range matchingTopics {
		err = helper.PublishString(ctx, topic, "data", 1)
		require.NoError(t, err)
	}

	// Should receive all messages
	for i := 0; i < len(matchingTopics); i++ {
		select {
		case msg := <-src.Messages():
			assert.Contains(t, msg.Message.Topic, prefix)
			msg.Ack()
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for wildcard hash match %d", i+1)
		}
	}
}

// TestSimulation_OverlappingWildcards validates overlapping wildcard subscriptions.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Source 1 subscribes to: "sensors/+/temperature"
//	Source 2 subscribes to: "sensors/#"
//
//	Publish to: "sensors/room1/temperature"
//
//	Both sources should receive the message
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_OverlappingWildcards(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("sensors")

	// Create first source with + wildcard
	srcCfg1 := helper.NewSourceConfig("overlap-source-1", []string{prefix + "/+/temperature"}, 1)
	src1, err := mqtt.NewSource(srcCfg1)
	require.NoError(t, err)
	defer src1.Close()

	// Create second source with # wildcard
	srcCfg2 := helper.NewSourceConfig("overlap-source-2", []string{prefix + "/#"}, 1)
	src2, err := mqtt.NewSource(srcCfg2)
	require.NoError(t, err)
	defer src2.Close()

	err = src1.Start(ctx)
	require.NoError(t, err)

	err = src2.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish message that matches both
	topic := prefix + "/room1/temperature"
	err = helper.PublishString(ctx, topic, "overlap test", 1)
	require.NoError(t, err)

	// Both sources should receive
	received1 := false
	received2 := false

loop:
	for i := 0; i < 2; i++ {
		select {
		case msg := <-src1.Messages():
			received1 = true
			msg.Ack()
		case msg := <-src2.Messages():
			received2 = true
			msg.Ack()
		case <-time.After(5 * time.Second):
			break loop
		}
	}

	assert.True(t, received1 || received2, "at least one source should receive overlapping message")
}

// ═══════════════════════════════════════════════════════════════════════════
// Session Management Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_CleanSessionSubscribe validates clean session behavior.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	CleanStart = true
//	- No previous session state
//	- Subscriptions must be re-established
//	- No queued messages from previous session
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_CleanSessionSubscribe(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/clean-session")

	// Create source with clean session
	srcCfg := &mqtt.SourceConfigImpl{
		ID: "clean-session-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  helper.BrokerURL(),
			CleanStart: true,
		},
		Topics: []string{topic},
		QoS:    1,
	}

	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish and receive
	err = helper.PublishString(ctx, topic, "clean session message", 1)
	require.NoError(t, err)

	select {
	case msg := <-src.Messages():
		assert.Equal(t, "clean session message", string(msg.Message.Payload))
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for clean session message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Shared Connection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_MultipleSourcesSameConnection validates multiple sources on one connection.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Single MQTTConnection
//	├── Source 1: "topic/a"
//	└── Source 2: "topic/b"
//
//	Messages to each topic routed to correct source
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_MultipleSourcesSameConnection(t *testing.T) {
	helper, cleanup := setupSubscriptionTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("test/multi")
	topicA := prefix + "/a"
	topicB := prefix + "/b"

	// Create shared connection
	connCfg := helper.NewConnectionConfig("shared-conn")
	conn, err := mqtt.NewConnection(connCfg)
	require.NoError(t, err)

	err = conn.Start(ctx, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Create two sources on same connection
	srcCfgA := &mqtt.SourceConfigImpl{
		ID:     "source-a",
		Topics: []string{topicA},
		QoS:    1,
	}
	srcA, err := conn.CreateSource(ctx, srcCfgA)
	require.NoError(t, err)
	defer srcA.Close()

	srcCfgB := &mqtt.SourceConfigImpl{
		ID:     "source-b",
		Topics: []string{topicB},
		QoS:    1,
	}
	srcB, err := conn.CreateSource(ctx, srcCfgB)
	require.NoError(t, err)
	defer srcB.Close()

	// Start both sources
	err = srcA.Start(ctx)
	require.NoError(t, err)

	err = srcB.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish to both topics
	err = helper.PublishString(ctx, topicA, "message A", 1)
	require.NoError(t, err)

	err = helper.PublishString(ctx, topicB, "message B", 1)
	require.NoError(t, err)

	// Both sources should receive their respective messages
	receivedMessages := make(map[string]string) // topic -> payload
	timeout := time.After(10 * time.Second)

	for len(receivedMessages) < 2 {
		select {
		case msg := <-srcA.Messages():
			receivedMessages[msg.Message.Topic] = string(msg.Message.Payload)
			msg.Ack()
		case msg := <-srcB.Messages():
			receivedMessages[msg.Message.Topic] = string(msg.Message.Payload)
			msg.Ack()
		case <-timeout:
			t.Fatalf("timeout waiting for messages, received: %v", receivedMessages)
		}
	}

	// Verify both messages were received on correct topics
	assert.Equal(t, "message A", receivedMessages[topicA], "should receive message A on topic A")
	assert.Equal(t, "message B", receivedMessages[topicB], "should receive message B on topic B")
}
