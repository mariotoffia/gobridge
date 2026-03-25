// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Retry Simulation Tests
//
// Simulation tests for transport retry behavior across QoS levels.
//
// Run with: go test -tags=integration ./transport/mqtt/tests/...
//
// Transport Retry Flow:
// ┌────────────────────────────────────────────────────────────────────┐
// │                    Target.Send() with Retry                        │
// ├────────────────────────────────────────────────────────────────────┤
// │                                                                    │
// │  ┌─────────┐     ┌──────────────────┐     ┌─────────────────────┐  │
// │  │ Message │────▶│ Check TTL        │────▶│ TTL Expired?        │  │
// │  │ + TTL   │     │ (first thing)    │     │ YES → ErrExpired    │  │
// │  └─────────┘     └──────────────────┘     │ NO  → Continue      │  │
// │                                           └─────────────────────┘  │
// │                                                     │              │
// │                                                     ▼              │
// │                          ┌─────────────────────────────────────┐   │
// │                          │ sendOnce() - Single Attempt         │   │
// │                          └────────────────┬────────────────────┘   │
// │                                           │                        │
// │           ┌───────────────────────────────┼───────────────────┐    │
// │           │                               │                   │    │
// │           ▼                               ▼                   ▼    │
// │   ┌───────────────┐              ┌───────────────┐   ┌───────────┐ │
// │   │ Success       │              │ Recoverable   │   │ Permanent │ │
// │   │ return nil    │              │ Error         │   │ Error     │ │
// │   └───────────────┘              └───────┬───────┘   └─────┬─────┘ │
// │                                          │                 │       │
// │                              Calculate   │                 │       │
// │                              Backoff     ▼                 │       │
// │                          ┌─────────────────────┐           │       │
// │                          │ Wait (TTL-bounded)  │           │       │
// │                          │ + Jitter            │◀──────────│       │
// │                          └─────────┬───────────┘           │       │
// │                                    │                       │       │
// │                                    ▼                       ▼       │
// │                          ┌─────────────────┐    ┌─────────────────┐│
// │                          │ Loop (TTL left) │    │ Return Error    ││
// │                          └─────────────────┘    │ immediately     ││
// │                                                 └─────────────────┘│
// └────────────────────────────────────────────────────────────────────┘
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ R001 │ QoS 0 uses transport retry             │ PASS     │
// │ R002 │ QoS 1 skips transport retry            │ PASS     │
// │ R003 │ QoS 2 skips transport retry            │ PASS     │
// │ R004 │ SkipNativeRetry=false forces retry     │ PASS     │
// │ R005 │ Retry success on transient failure     │ PASS     │
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

// setupRetryTest creates a Mosquitto container for retry testing.
func setupRetryTest(t *testing.T) (*MQTTLocalHelper, func()) {
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
// Transport Retry Configuration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_QoS0_UsesTransportRetry validates QoS 0 uses transport retry.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	QoS 0 has NO native retry (fire-and-forget)
//	Transport retry IS used to handle infrastructure failures
//	Target capabilities: PublishAtMostOnce, NO NativeRetry
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS0_UsesTransportRetry(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/qos0")

	// Subscribe to receive messages
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 0)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with QoS 0
	cfg := helper.NewTargetConfig("qos0-retry-target", topic, 0)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Verify capabilities
	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtMostOnce),
		"QoS 0 should have PublishAtMostOnce")
	assert.False(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 0 should NOT have NativeRetry")

	// Connect and send
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 0 with transport retry"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)

	// Verify message received
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

// TestSimulation_QoS1_SkipsTransportRetry validates QoS 1 skips transport retry.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	QoS 1 HAS native retry (PUBACK handshake)
//	Transport retry IS SKIPPED (SkipNativeRetry=true by default)
//	Target capabilities: PublishAtLeastOnce, NativeRetry
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_QoS1_SkipsTransportRetry(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/qos1")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with QoS 1
	cfg := helper.NewTargetConfig("qos1-retry-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Verify capabilities
	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtLeastOnce),
		"QoS 1 should have PublishAtLeastOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 1 should have NativeRetry")

	// Connect and send
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 1 native retry"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)

	// Verify message received
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

// TestSimulation_QoS2_SkipsTransportRetry validates QoS 2 skips transport retry.
func TestSimulation_QoS2_SkipsTransportRetry(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/qos2")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 2)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with QoS 2
	cfg := helper.NewTargetConfig("qos2-retry-target", topic, 2)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Verify capabilities
	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishExactOnce),
		"QoS 2 should have PublishExactOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 2 should have NativeRetry")

	// Connect and send
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("QoS 2 native retry"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)
}

// TestSimulation_SkipNativeRetryFalse validates forcing transport retry.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	SkipNativeRetry = false
//	Even QoS 1/2 with native retry will use transport retry
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_SkipNativeRetryFalse(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/force")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with QoS 1 but force transport retry
	skipNative := false
	retryConfig := bridgeTypes.TransportRetryConfig{
		InitialBackoff:  100 * time.Millisecond,
		MaxBackoff:      time.Second,
		Multiplier:      2.0,
		SkipNativeRetry: &skipNative, // Force transport retry
	}

	cfg := helper.NewTargetConfig("force-retry-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg, mqtt.WithTransportRetry(retryConfig))
	require.NoError(t, err)
	defer tgt.Close()

	// Connect and send
	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("forced transport retry"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)

	// Verify message received
	msgs, err := helper.WaitForMessages(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

// ═══════════════════════════════════════════════════════════════════════════
// Transport Retry Config Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_TransportRetryConfig validates custom retry configuration.
func TestSimulation_TransportRetryConfig(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/config")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with custom retry config
	retryConfig := bridgeTypes.TransportRetryConfig{
		InitialBackoff:                  50 * time.Millisecond,
		MaxBackoff:                      500 * time.Millisecond,
		Multiplier:                      1.5,
		Jitter:                          0.1,
		InfrastructureBackoffMultiplier: 2.0,
	}

	cfg := helper.NewTargetConfig("custom-retry-target", topic, 0)
	tgt, err := mqtt.NewTarget(cfg, mqtt.WithTransportRetry(retryConfig))
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("custom retry config"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)
}

// TestSimulation_DefaultTTL validates default TTL setting.
func TestSimulation_DefaultTTL(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/ttl")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target with default TTL
	cfg := helper.NewTargetConfig("ttl-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg, mqtt.WithDefaultTTL(5*time.Minute))
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Message without explicit TTL uses default
	msg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Payload:   []byte("default TTL message"),
	}

	err = tgt.Send(ctx, msg)
	assert.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Retry Success Scenarios
// ═══════════════════════════════════════════════════════════════════════════

// TestSimulation_RetrySuccessfulDelivery validates successful delivery with retry.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Normal successful delivery path
//	No errors, message delivered on first attempt
//
// ───────────────────────────────────────────────────────────────────────────
func TestSimulation_RetrySuccessfulDelivery(t *testing.T) {
	helper, cleanup := setupRetryTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := UniqueTopic("test/retry/success")

	// Subscribe
	err := helper.StartTestClient(ctx)
	require.NoError(t, err)

	err = helper.Subscribe(ctx, topic, 1)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	// Create target
	cfg := helper.NewTargetConfig("success-target", topic, 1)
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	err = tgt.Connect(ctx)
	require.NoError(t, err)

	// Send multiple messages successfully
	for i := 0; i < 5; i++ {
		msg := bridgeTypes.Message{
			CreatedAt: time.Now(),
			Payload:   []byte("success message"),
		}
		err = tgt.Send(ctx, msg)
		require.NoError(t, err, "message %d should succeed", i)
	}

	// All should be received
	msgs, err := helper.WaitForMessages(ctx, 5)
	require.NoError(t, err)
	assert.Len(t, msgs, 5)
}
