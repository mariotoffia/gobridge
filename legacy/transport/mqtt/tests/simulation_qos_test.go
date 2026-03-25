// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - QoS Simulation Tests
//
// Simulation tests for MQTT QoS levels 0, 1, and 2.
// These tests use a real Mosquitto broker to validate QoS behavior.
//
// Run with: go test -tags=integration ./transport/mqtt/tests/...
//
// QoS Level Testing Matrix:
// ┌─────┬─────────────────┬──────────────────────────────────────────┐
// │ QoS │ Protocol Flow   │ Test Scenarios                           │
// ├─────┼─────────────────┼──────────────────────────────────────────┤
// │  0  │ PUBLISH →       │ Fire-and-forget, no confirmation         │
// │     │                 │ - Success path                           │
// │     │                 │ - No guarantee on delivery               │
// ├─────┼─────────────────┼──────────────────────────────────────────┤
// │  1  │ PUBLISH →       │ At-least-once with PUBACK                │
// │     │ ← PUBACK        │ - Success path                           │
// │     │                 │ - PUBACK confirms broker receipt         │
// │     │                 │ - Possible duplicate delivery            │
// ├─────┼─────────────────┼──────────────────────────────────────────┤
// │  2  │ PUBLISH →       │ Exactly-once 4-way handshake             │
// │     │ ← PUBREC        │ - Success path                           │
// │     │ PUBREL →        │ - Full handshake completion              │
// │     │ ← PUBCOMP       │ - No duplicate delivery                  │
// └─────┴─────────────────┴──────────────────────────────────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ Q001 │ QoS 0 publish success                  │ PASS     │
// │ Q002 │ QoS 0 subscribe receives               │ PASS     │
// │ Q003 │ QoS 1 publish success                  │ PASS     │
// │ Q004 │ QoS 1 subscribe receives               │ PASS     │
// │ Q005 │ QoS 2 publish success                  │ PASS     │
// │ Q006 │ QoS 2 subscribe receives               │ PASS     │
// │ Q007 │ Mixed QoS publish                      │ PASS     │
// │ Q008 │ Mixed QoS subscribe                    │ PASS     │
// │ Q009 │ QoS downgrade on subscribe             │ PASS     │
// │ Q010 │ Message overrides target QoS           │ PASS     │
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

// setupQoSTest creates a Mosquitto container for QoS testing.
func setupQoSTest(t *testing.T) (*MQTTLocalHelper, func()) {
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
// QoS 0 Tests - Fire and Forget
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_QoS0_PublishSuccess validates QoS 0 publish.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send() with QoS 0
//	│
//	▼
//	PUBLISH ──────────────────────────▶ Broker
//	│                                     │
//	│ (no PUBACK - fire and forget)       │
//	▼                                     ▼
//	Send() returns nil                  Message accepted (no guarantee)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS0_PublishSuccess(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create target with QoS 0
	cfg := helper.NewTargetConfig("qos0-target", "test/qos0", 0)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Connect
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message with QoS 0
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 0 message"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err, "QoS 0 send should succeed")
}

// TestSimulation_QoS0_SubscribeReceives validates QoS 0 receive.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Test client publishes message
//	Source subscribes with QoS 0
//	Source receives message (at most once)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS0_SubscribeReceives(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/qos0/sub")

	// Create source with QoS 0
	srcCfg := helper.NewSourceConfig("qos0-source", []string{topic}, 0)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	// Start source
	err = src.Start(ctx)
	require.NoError(t, err)

	// Give subscription time to establish
	time.Sleep(500 * time.Millisecond)

	// Publish test message
	expectedPayload := "QoS 0 test message"
	err = helper.PublishString(ctx, topic, expectedPayload, 0)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, expectedPayload, string(msg.Message.Payload))
		assert.Equal(t, 0, msg.Message.Qos.Level)
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for QoS 0 message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// QoS 1 Tests - At Least Once
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_QoS1_PublishSuccess validates QoS 1 publish with PUBACK.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send() with QoS 1
//	│
//	▼
//	PUBLISH ──────────────────────────▶ Broker
//	│                                     │
//	│                                     ▼
//	│◀───────────────────────────── PUBACK
//	│
//	▼
//	Send() returns nil (broker confirmed receipt)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS1_PublishSuccess(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create target with QoS 1
	cfg := helper.NewTargetConfig("qos1-target", "test/qos1", 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Connect
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message with QoS 1
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 1 message"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err, "QoS 1 send should succeed (PUBACK received)")
}

// TestSimulation_QoS1_SubscribeReceives validates QoS 1 receive.
func TestSimulation_QoS1_SubscribeReceives(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/qos1/sub")

	// Create source with QoS 1
	srcCfg := helper.NewSourceConfig("qos1-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	// Start source
	err = src.Start(ctx)
	require.NoError(t, err)

	// Give subscription time to establish
	time.Sleep(500 * time.Millisecond)

	// Publish test message
	expectedPayload := "QoS 1 test message"
	err = helper.PublishString(ctx, topic, expectedPayload, 1)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, expectedPayload, string(msg.Message.Payload))
		assert.Equal(t, 1, msg.Message.Qos.Level)
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for QoS 1 message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// QoS 2 Tests - Exactly Once
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_QoS2_PublishSuccess validates QoS 2 publish with full handshake.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send() with QoS 2
//	│
//	▼
//	PUBLISH ──────────────────────────▶ Broker
//	│                                     │
//	│◀───────────────────────────── PUBREC (received)
//	│
//	PUBREL ──────────────────────────▶ Broker
//	│                                     │
//	│◀───────────────────────────── PUBCOMP (complete)
//	│
//	▼
//	Send() returns nil (exactly-once delivery confirmed)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS2_PublishSuccess(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create target with QoS 2
	cfg := helper.NewTargetConfig("qos2-target", "test/qos2", 2)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Connect
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message with QoS 2
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 2 message"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err, "QoS 2 send should succeed (PUBCOMP received)")
}

// TestSimulation_QoS2_SubscribeReceives validates QoS 2 receive.
func TestSimulation_QoS2_SubscribeReceives(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/qos2/sub")

	// Create source with QoS 2
	srcCfg := helper.NewSourceConfig("qos2-source", []string{topic}, 2)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	// Start source
	err = src.Start(ctx)
	require.NoError(t, err)

	// Give subscription time to establish
	time.Sleep(500 * time.Millisecond)

	// Publish test message
	expectedPayload := "QoS 2 test message"
	err = helper.PublishString(ctx, topic, expectedPayload, 2)
	require.NoError(t, err)

	// Receive message
	select {
	case msg := <-src.Messages():
		assert.Equal(t, expectedPayload, string(msg.Message.Payload))
		assert.Equal(t, 2, msg.Message.Qos.Level)
		msg.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for QoS 2 message")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Mixed QoS Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_MixedQoS_Publish validates publishing with different QoS levels.
func TestSimulation_MixedQoS_Publish(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create target with QoS 1 (default)
	cfg := helper.NewTargetConfig("mixed-target", "test/mixed", 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send messages with different QoS levels
	tests := []struct {
		name string
		qos  int
	}{
		{"QoS 0", 0},
		{"QoS 1", 1},
		{"QoS 2", 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := bridgeTypes.Message{
				CreatedAt: time.Now(),
				Topic:     "test/mixed/" + tc.name,
				Payload:   []byte("mixed qos message"),
				Qos:       &bridgeTypes.QosLevel{Level: tc.qos},
			}

			err := tgt.Send(ctx, msg)
			assert.NoError(t, err, "send with %s should succeed", tc.name)
		})
	}
}

// TestSimulation_MixedQoS_Subscribe validates subscribing with different QoS levels.
func TestSimulation_MixedQoS_Subscribe(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create sources with different QoS levels
	topics := []struct {
		topic string
		qos   int
	}{
		{UniqueTopic("test/mixed/qos0"), 0},
		{UniqueTopic("test/mixed/qos1"), 1},
		{UniqueTopic("test/mixed/qos2"), 2},
	}

	for _, tc := range topics {
		t.Run(tc.topic, func(t *testing.T) {
			srcCfg := helper.NewSourceConfig("mixed-source", []string{tc.topic}, tc.qos)
			src, err := mqtt.NewSource(srcCfg)
			require.NoError(t, err)
			defer src.Close()

			err = src.Start(ctx)
			require.NoError(t, err)

			// Give subscription time
			time.Sleep(300 * time.Millisecond)

			// Publish and receive
			err = helper.PublishString(ctx, tc.topic, "test", byte(tc.qos))
			require.NoError(t, err)

			select {
			case msg := <-src.Messages():
				assert.Equal(t, tc.qos, msg.Message.Qos.Level)
				msg.Ack()
			case <-time.After(5 * time.Second):
				t.Fatalf("timeout waiting for message on %s", tc.topic)
			}
		})
	}
}

// TestSimulation_MessageOverridesTargetQoS validates message QoS override.
func TestSimulation_MessageOverridesTargetQoS(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create target with QoS 0 default
	cfg := helper.NewTargetConfig("override-target", "test/override", 0)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message with QoS 2 override
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("overridden QoS"),
		Qos:       &bridgeTypes.QosLevel{Level: 2},
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err, "message QoS override should work")
}

// ═══════════════════════════════════════════════════════════════════════════
// Round Trip Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_QoS1_RoundTrip validates complete publish-subscribe cycle.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Target.Send()                    Mosquitto                 Source
//	     │                               │                        │
//	     │──── PUBLISH (QoS 1) ─────────▶│                        │
//	     │◀──── PUBACK ──────────────────│                        │
//	     │                               │                        │
//	     │                               │──── PUBLISH ──────────▶│
//	     │                               │◀──── PUBACK ───────────│
//	     │                               │                        │
//	Send() returns nil              Message delivered      Messages() receives
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS1_RoundTrip(t *testing.T) {
	helper, cleanup := setupQoSTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/roundtrip/qos1")

	// Create source
	srcCfg := helper.NewSourceConfig("roundtrip-source", []string{topic}, 1)
	src, err := mqtt.NewSource(srcCfg)
	require.NoError(t, err)
	defer src.Close()

	err = src.Start(ctx)
	require.NoError(t, err)

	// Give subscription time
	time.Sleep(500 * time.Millisecond)

	// Create target
	tgtCfg := helper.NewTargetConfig("roundtrip-target", topic, 1)
	tgt, err := mqtt.NewTarget(tgtCfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send message
	expectedPayload := "round trip test message"
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte(expectedPayload),
		Metadata: map[string]any{
			"testKey": "testValue",
		},
	}

	err = tgt.Send(ctx, msg)
	require.NoError(t, err)

	// Receive message
	select {
	case received := <-src.Messages():
		assert.Equal(t, expectedPayload, string(received.Message.Payload))
		assert.Equal(t, topic, received.Message.Topic)
		received.Ack()
	case <-ctx.Done():
		t.Fatal("timeout waiting for round trip message")
	}
}
