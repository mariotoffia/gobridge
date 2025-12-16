// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Integration Tests
//
// Integration tests using Mosquitto for end-to-end MQTT operations.
// These tests require Docker to be running with Mosquitto.
//
// Run with: go test -tags=integration ./transport/mqtt/tests/...
//
// Test Flow:
// ┌──────────────────────────────────────────────────────────────────────────┐
// │                       Mosquitto MQTT Integration                         │
// ├──────────────────────────────────────────────────────────────────────────┤
// │                                                                          │
// │   ┌─────────────┐      ┌─────────────────┐      ┌─────────────┐          │
// │   │   Target    │─────▶│   Mosquitto     │─────▶│   Source    │          │
// │   │   .Send()   │      │    Broker       │      │ .Messages() │          │
// │   └─────────────┘      └─────────────────┘      └─────────────┘          │
// │                                                                          │
// └──────────────────────────────────────────────────────────────────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ I001 │ Source receives messages               │ PASS     │
// │ I002 │ Target sends messages                  │ PASS     │
// │ I003 │ Source-Target round trip               │ PASS     │
// │ I004 │ Wildcard subscription                  │ PASS     │
// │ I005 │ Retained messages                      │ PASS     │
// │ I006 │ Shared connection mode                 │ PASS     │
// │ I007 │ Standalone connection mode             │ PASS     │
// │ I008 │ Graceful shutdown                      │ PASS     │
// │ I009 │ Connection lifecycle                   │ PASS     │
// │ I010 │ Multiple QoS levels                    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

package mqtttests

import (
	"context"
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

// setupIntegrationTest creates a Mosquitto container for integration testing.
func setupIntegrationTest(t *testing.T) (*MQTTLocalHelper, func()) {
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
// Source Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_Source_ReceiveMessages validates Source receives messages.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Start Source subscription
//  2. Publish test message via helper
//  3. Receive message on Source channel
//  4. Verify payload
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_MQTT_Source_ReceiveMessages(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/source/receive")
	expectedBody := "Hello, MQTT!"

	// Create and start source
	srcCfg := helper.NewSourceConfig("test-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Give subscription time to establish
	time.Sleep(500 * time.Millisecond)

	// Publish test message
	err = helper.PublishString(ctx, topic, expectedBody, 1)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, expectedBody, string(msg.Message.Payload))
		assert.Equal(t, topic, msg.Message.Topic)
		err := msg.Ack()
		assert.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_Target_SendMessage validates Target sends message.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//  1. Subscribe via helper
//  2. Send message via Target
//  3. Verify message received
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_MQTT_Target_SendMessage(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/target/send")

	// Subscribe via helper
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target
	tgtCfg := helper.NewTargetConfig("test-target", topic, 1)
	tgt, err := mqtt.NewTarget(tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Connect and send
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("Hello from Target!"),
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Verify message received
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Hello from Target!", string(msgs[0].Payload))
}

// ═══════════════════════════════════════════════════════════════════════════
// Round Trip Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_RoundTrip validates full Source→Target flow.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send()                    Mosquitto                Source.Messages()
//	     │                               │                          │
//	     │──── PUBLISH ─────────────────▶│                          │
//	     │◀──── PUBACK ──────────────────│                          │
//	     │                               │──── PUBLISH ────────────▶│
//	     │                               │◀──── PUBACK ─────────────│
//	     │                               │                          │
//	Send() returns nil              Message delivered      Messages() receives
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_MQTT_RoundTrip(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/roundtrip")

	// Create source
	srcCfg := helper.NewSourceConfig("roundtrip-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Create target
	tgtCfg := helper.NewTargetConfig("roundtrip-target", topic, 1)
	tgt, err := mqtt.NewTarget(tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message via target
	expectedPayload := "Round trip test message"
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte(expectedPayload),
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Receive via source
	select {
	case received := <-src.Messages():
		assert.Equal(t, expectedPayload, string(received.Message.Payload))
		assert.Equal(t, topic, received.Message.Topic)
		err := received.Ack()
		assert.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for round trip message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Wildcard Subscription Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_WildcardSubscription validates wildcard pattern matching.
func TestIntegration_MQTT_WildcardSubscription(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("sensors")
	wildcard := prefix + "/+/temperature"

	// Create source with wildcard
	srcCfg := helper.NewSourceConfig("wildcard-source", []string{wildcard}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Publish to matching topics
	topics := []string{
		prefix + "/room1/temperature",
		prefix + "/room2/temperature",
	}

	for _, topic := range topics {
		err = helper.PublishString(ctx, topic, "25.5", 1)
		require.NoError(t, err)
	}

	// Should receive both messages
	received := 0
	timeout := time.After(5 * time.Second)
	for received < 2 {
		select {
		case msg := <-src.Messages():
			received++
			msg.Ack()
		case <-timeout:
			t.Fatalf("only received %d of 2 messages", received)
		}
	}

	assert.Equal(t, 2, received)
}

// ═══════════════════════════════════════════════════════════════════════════
// Retained Message Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_RetainedMessages validates retained message behavior.
func TestIntegration_MQTT_RetainedMessages(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retained")

	// Publish retained message first
	err := helper.PublishRetained(ctx, topic, []byte("retained value"), 1)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// New subscriber should receive retained message
	srcCfg := helper.NewSourceConfig("retained-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Should receive retained message immediately
	select {
	case msg := <-src.Messages():
		assert.Equal(t, "retained value", string(msg.Message.Payload))
		msg.Ack()
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for retained message")
	}

	// Clean up
	err = helper.ClearRetained(ctx, topic)
	require.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Connection Mode Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_SharedConnection validates shared connection mode.
func TestIntegration_MQTT_SharedConnection(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := UniqueTopic("test/shared")
	topicA := prefix + "/a"
	topicB := prefix + "/b"

	// Create shared connection
	connCfg := helper.NewConnectionConfig("shared-test-conn")
	conn, err := mqtt.NewConnection(connCfg)
	require.NoError(t, err)

	err = conn.Start(ctx, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Create source on shared connection
	srcCfg := &mqtt.SourceConfigImpl{
		ID:     "shared-source",
		Topics: []string{topicA},
		QoS:    1,
	}
	src, err := conn.CreateSource(ctx, srcCfg)
	require.NoError(t, err)
	defer src.Close()

	// Create target on shared connection
	tgtCfg := &mqtt.TargetConfigImpl{
		ID:           "shared-target",
		DefaultTopic: topicB,
		QoS:          1,
	}
	tgt, err := conn.CreateTarget(ctx, tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Start source
	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Publish via helper to source topic
	err = helper.PublishString(ctx, topicA, "to source", 1)
	require.NoError(t, err)

	// Source should receive
	select {
	case msg := <-src.Messages():
		assert.Equal(t, "to source", string(msg.Message.Payload))
		msg.Ack()
	case <-time.After(5 * time.Second):
		t.Fatal("source didn't receive message")
	}

	// Target can also send
	err = tgt.Send(ctx, bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("from target"),
	})
	assert.NoError(t, err)
}

// TestIntegration_MQTT_StandaloneConnection validates standalone mode.
func TestIntegration_MQTT_StandaloneConnection(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/standalone")

	// Create standalone source (manages its own connection)
	srcCfg := helper.NewSourceConfig("standalone-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	// Create standalone target (manages its own connection)
	tgtCfg := helper.NewTargetConfig("standalone-target", topic, 1)
	tgt, err := mqtt.NewTarget(tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Start source
	err = src.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	// Connect target
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send and receive
	err = tgt.Send(ctx, bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("standalone message"),
	})
	require.NoError(t, err)

	select {
	case msg := <-src.Messages():
		assert.Equal(t, "standalone message", string(msg.Message.Payload))
		msg.Ack()
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for standalone message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Graceful Shutdown Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_GracefulShutdown validates graceful shutdown.
func TestIntegration_MQTT_GracefulShutdown(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	topic := UniqueTopic("test/shutdown")

	// Create source
	srcCfg := helper.NewSourceConfig("shutdown-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
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

// ═══════════════════════════════════════════════════════════════════════════
// Connection Lifecycle Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_ConnectionLifecycle validates connection lifecycle.
func TestIntegration_MQTT_ConnectionLifecycle(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create connection
	connCfg := helper.NewConnectionConfig("lifecycle-conn")
	conn, err := mqtt.NewConnection(connCfg)
	require.NoError(t, err)

	// Initially not running
	assert.False(t, conn.IsRunning())

	// Start connection
	err = conn.Start(ctx, nil)
	require.NoError(t, err)
	assert.True(t, conn.IsRunning())

	// Create source/target works after start
	srcCfg := &mqtt.SourceConfigImpl{
		ID:     "lifecycle-source",
		Topics: []string{"test/lifecycle"},
		QoS:    1,
	}
	src, err := conn.CreateSource(ctx, srcCfg)
	require.NoError(t, err)
	defer src.Close()

	tgtCfg := &mqtt.TargetConfigImpl{
		ID:           "lifecycle-target",
		DefaultTopic: "test/lifecycle",
		QoS:          1,
	}
	tgt, err := conn.CreateTarget(ctx, tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Close connection
	err = conn.Close()
	assert.NoError(t, err)
	assert.False(t, conn.IsRunning())
}

// ═══════════════════════════════════════════════════════════════════════════
// Multiple QoS Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_MQTT_MultipleQoSLevels validates all QoS levels work.
func TestIntegration_MQTT_MultipleQoSLevels(t *testing.T) {
	helper, cleanup := setupIntegrationTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for qos := 0; qos <= 2; qos++ {
		t.Run("QoS"+string(rune('0'+qos)), func(t *testing.T) {
			topic := UniqueTopic("test/qos/" + string(rune('0'+qos)))

			// Create source
			srcCfg := helper.NewSourceConfig("qos-source", []string{topic}, qos)
			src, err := mqtt.NewSource(srcCfg)
			require.NoError(t, err)
			defer src.Close()

			err = src.Start(ctx)
			require.NoError(t, err)

			time.Sleep(300 * time.Millisecond)

			// Create target
			tgtCfg := helper.NewTargetConfig("qos-target", topic, qos)
			tgt, err := mqtt.NewTarget(tgtCfg)
			require.NoError(t, err)
			defer tgt.Close()

			err = tgt.Connect(ctx)
			require.NoError(t, err)

			// Send and receive
			msg := bridgeTypes.Message{
				CreatedAt: time.Now(),
				Payload:   []byte("qos test"),
			}
			err = tgt.Send(ctx, msg)
			require.NoError(t, err)

			select {
			case received := <-src.Messages():
				assert.Equal(t, "qos test", string(received.Message.Payload))
				received.Ack()
			case <-time.After(5 * time.Second):
				t.Fatalf("QoS %d: timeout", qos)
			}
		})
	}
}
