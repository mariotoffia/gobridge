// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Target Internal Unit Tests
//
// # Tests for unexported methods in target.go
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ TI001│ shouldUseTransportRetry QoS 0          │ PASS     │
// │ TI002│ shouldUseTransportRetry QoS 1          │ PASS     │
// │ TI003│ shouldUseTransportRetry QoS 2          │ PASS     │
// │ TI004│ shouldUseTransportRetry force enabled  │ PASS     │
// │ TI005│ hasNativeRetry QoS 0                   │ PASS     │
// │ TI006│ hasNativeRetry QoS 1                   │ PASS     │
// │ TI007│ hasNativeRetry QoS 2                   │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtt

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// shouldUseTransportRetry Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_ShouldUseTransportRetry_QoS0 validates QoS 0 uses transport retry.
//
// QoS 0 (fire-and-forget) has NO native retry mechanism.
// Transport retry SHOULD be used to handle infrastructure failures.
func TestTarget_ShouldUseTransportRetry_QoS0(t *testing.T) {
	tgt := &Target{
		qos:            0,
		transportRetry: types.DefaultTransportRetryConfig(),
	}

	assert.True(t, tgt.shouldUseTransportRetry(),
		"QoS 0 should use transport retry (no native retry)")
}

// TestTarget_ShouldUseTransportRetry_QoS1 validates QoS 1 skips transport retry.
//
// QoS 1 (at-least-once) has native PUBACK retry.
// Transport retry should be SKIPPED (SkipNativeRetry=true by default).
func TestTarget_ShouldUseTransportRetry_QoS1(t *testing.T) {
	tgt := &Target{
		qos:            1,
		transportRetry: types.DefaultTransportRetryConfig(),
	}

	assert.False(t, tgt.shouldUseTransportRetry(),
		"QoS 1 should skip transport retry (has native retry)")
}

// TestTarget_ShouldUseTransportRetry_QoS2 validates QoS 2 skips transport retry.
//
// QoS 2 (exactly-once) has native PUBREC/PUBREL/PUBCOMP handshake.
// Transport retry should be SKIPPED (SkipNativeRetry=true by default).
func TestTarget_ShouldUseTransportRetry_QoS2(t *testing.T) {
	tgt := &Target{
		qos:            2,
		transportRetry: types.DefaultTransportRetryConfig(),
	}

	assert.False(t, tgt.shouldUseTransportRetry(),
		"QoS 2 should skip transport retry (has native retry)")
}

// TestTarget_ShouldUseTransportRetry_ForceEnabled validates SkipNativeRetry=false.
//
// When SkipNativeRetry=false, transport retry is ALWAYS used,
// even for QoS 1/2 which have native retry.
func TestTarget_ShouldUseTransportRetry_ForceEnabled(t *testing.T) {
	skipNative := false
	tgt := &Target{
		qos: 1,
		transportRetry: types.TransportRetryConfig{
			SkipNativeRetry: &skipNative,
		},
	}

	assert.True(t, tgt.shouldUseTransportRetry(),
		"SkipNativeRetry=false should force transport retry")
}

// ═══════════════════════════════════════════════════════════════════════════
// hasNativeRetry Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_HasNativeRetry_QoS0 validates QoS 0 has no native retry.
//
// QoS 0 is fire-and-forget - no broker acknowledgment or retry.
func TestTarget_HasNativeRetry_QoS0(t *testing.T) {
	tgt := &Target{qos: 0}
	assert.False(t, tgt.hasNativeRetry(),
		"QoS 0 should have no native retry")
}

// TestTarget_HasNativeRetry_QoS1 validates QoS 1 has native retry.
//
// QoS 1 waits for PUBACK and broker handles retransmission.
func TestTarget_HasNativeRetry_QoS1(t *testing.T) {
	tgt := &Target{qos: 1}
	assert.True(t, tgt.hasNativeRetry(),
		"QoS 1 should have native retry")
}

// TestTarget_HasNativeRetry_QoS2 validates QoS 2 has native retry.
//
// QoS 2 uses 4-way handshake with broker retransmission.
func TestTarget_HasNativeRetry_QoS2(t *testing.T) {
	tgt := &Target{qos: 2}
	assert.True(t, tgt.hasNativeRetry(),
		"QoS 2 should have native retry")
}

// ═══════════════════════════════════════════════════════════════════════════
// Capabilities Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_Capabilities_QoS0_NoNativeRetry validates QoS 0 capabilities.
func TestTarget_Capabilities_QoS0_NoNativeRetry(t *testing.T) {
	tgt := &Target{qos: 0}
	caps := tgt.Capabilities()

	assert.True(t, caps.Has(types.CapabilityPublishAtMostOnce),
		"QoS 0 should have PublishAtMostOnce")
	assert.False(t, caps.Has(types.CapabilityNativeRetry),
		"QoS 0 should NOT have NativeRetry")
}

// TestTarget_Capabilities_QoS1_HasNativeRetry validates QoS 1 capabilities.
func TestTarget_Capabilities_QoS1_HasNativeRetry(t *testing.T) {
	tgt := &Target{qos: 1}
	caps := tgt.Capabilities()

	assert.True(t, caps.Has(types.CapabilityPublishAtLeastOnce),
		"QoS 1 should have PublishAtLeastOnce")
	assert.True(t, caps.Has(types.CapabilityNativeRetry),
		"QoS 1 should have NativeRetry")
}

// TestTarget_Capabilities_QoS2_HasNativeRetry validates QoS 2 capabilities.
func TestTarget_Capabilities_QoS2_HasNativeRetry(t *testing.T) {
	tgt := &Target{qos: 2}
	caps := tgt.Capabilities()

	assert.True(t, caps.Has(types.CapabilityPublishExactOnce),
		"QoS 2 should have PublishExactOnce")
	assert.True(t, caps.Has(types.CapabilityNativeRetry),
		"QoS 2 should have NativeRetry")
}

// ═══════════════════════════════════════════════════════════════════════════
// Option Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestWithTransportRetry validates the option function.
func TestWithTransportRetry(t *testing.T) {
	cfg := types.TransportRetryConfig{
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     1.5,
	}

	tgt := &Target{}
	opt := WithTransportRetry(cfg)
	opt(tgt)

	assert.Equal(t, 500*time.Millisecond, tgt.transportRetry.InitialBackoff)
	assert.Equal(t, 30*time.Second, tgt.transportRetry.MaxBackoff)
	assert.Equal(t, 1.5, tgt.transportRetry.Multiplier)
}

// TestWithDefaultTTL validates the option function.
func TestWithDefaultTTL(t *testing.T) {
	tgt := &Target{}
	opt := WithDefaultTTL(5 * time.Minute)
	opt(tgt)

	assert.Equal(t, 5*time.Minute, tgt.defaultTTL)
}
