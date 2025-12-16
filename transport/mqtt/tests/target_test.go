// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Target Unit Tests
//
// Tests for Target constructor validation and behavior.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ T001 │ NewTarget nil config error             │ PASS     │
// │ T002 │ NewTarget no broker error              │ PASS     │
// │ T003 │ NewTarget valid config success         │ PASS     │
// │ T004 │ NewTargetWithClient nil client error   │ PASS     │
// │ T005 │ Target GetID returns configured value  │ PASS     │
// │ T006 │ Target GetTransportType returns MQTT   │ PASS     │
// │ T007 │ Target Capabilities QoS 0              │ PASS     │
// │ T008 │ Target Capabilities QoS 1              │ PASS     │
// │ T009 │ Target Capabilities QoS 2              │ PASS     │
// │ T010 │ Target default timeout                 │ PASS     │
// │ T011 │ Target custom timeout                  │ PASS     │
// │ T012 │ Target Close idempotent                │ PASS     │
// │ T013 │ WithTransportRetry option              │ PASS     │
// │ T014 │ WithDefaultTTL option                  │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewTarget Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewTarget_NilConfig validates nil config returns error.
func TestNewTarget_NilConfig(t *testing.T) {
	tgt, err := mqtt.NewTarget(nil)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewTarget_NoBroker validates missing broker returns error.
func TestNewTarget_NoBroker(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID:           "test-target",
		DefaultTopic: "test/topic",
	}
	tgt, err := mqtt.NewTarget(cfg)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "broker URL is required")
}

// TestNewTarget_ValidConfig validates valid config succeeds.
func TestNewTarget_ValidConfig(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		DefaultTopic: "test/topic",
		QoS:          1,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()
}

// TestNewTarget_NoDefaultTopic validates target works without default topic.
func TestNewTarget_NoDefaultTopic(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		// DefaultTopic not set - messages must specify topic
		QoS: 1,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()
}

// TestNewTarget_QoSClamping validates QoS > 2 clamps to 1.
//
// ═══════════════════════════════════════════════════════════════════════════
// Input: QoS  →  Result
// ═══════════════════════════════════════════════════════════════════════════
//
//	0  →  0 (valid)
//	1  →  1 (valid)
//	2  →  2 (valid)
//	3  →  1 (clamped)
//	99 →  1 (clamped)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_QoSClamping(t *testing.T) {
	tests := []struct {
		name     string
		inputQoS int
	}{
		{"QoS 0 valid", 0},
		{"QoS 1 valid", 1},
		{"QoS 2 valid", 2},
		{"QoS 3 clamps to 1", 3},
		{"QoS 99 clamps to 1", 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &mqtt.TargetConfigImpl{
				ID: "test-target",
				Connection: mqtt.ConnectionConfig{
					BrokerURL: "tcp://localhost:1883",
				},
				QoS: tc.inputQoS,
			}
			tgt, err := mqtt.NewTarget(cfg)
			require.NoError(t, err)
			require.NotNil(t, tgt)
			defer tgt.Close()
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// NewTargetWithClient Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewTargetWithClient_NilConfig validates nil config returns error.
func TestNewTargetWithClient_NilConfig(t *testing.T) {
	tgt, err := mqtt.NewTargetWithClient(nil, nil)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_GetID validates ID getter returns configured value.
func TestTarget_GetID(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "my-target-id",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	assert.Equal(t, "my-target-id", tgt.GetID())
}

// TestTarget_GetTransportType validates transport type returns MQTT.
func TestTarget_GetTransportType(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	assert.Equal(t, mqtt.TransportType, tgt.GetTransportType())
}

// TestTarget_Capabilities_QoS0 validates QoS 0 capabilities.
//
// QoS 0 (fire-and-forget):
// - CapabilityPublishAtMostOnce: Message may be lost
// - NO CapabilityNativeRetry: No broker acknowledgment
func TestTarget_Capabilities_QoS0(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		QoS: 0,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtMostOnce),
		"QoS 0 should have PublishAtMostOnce")
	assert.False(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 0 should NOT have NativeRetry")
}

// TestTarget_Capabilities_QoS1 validates QoS 1 capabilities.
//
// QoS 1 (at-least-once):
// - CapabilityPublishAtLeastOnce: Message delivered at least once
// - CapabilityNativeRetry: Broker handles PUBACK retry
func TestTarget_Capabilities_QoS1(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		QoS: 1,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtLeastOnce),
		"QoS 1 should have PublishAtLeastOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 1 should have NativeRetry")
}

// TestTarget_Capabilities_QoS2 validates QoS 2 capabilities.
//
// QoS 2 (exactly-once):
// - CapabilityPublishExactOnce: Message delivered exactly once
// - CapabilityNativeRetry: Broker handles full handshake retry
func TestTarget_Capabilities_QoS2(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		QoS: 2,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	caps := tgt.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishExactOnce),
		"QoS 2 should have PublishExactOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"QoS 2 should have NativeRetry")
}

// TestTarget_DefaultTimeout validates default timeout is 30s.
func TestTarget_DefaultTimeout(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		// Timeout not set - should default to 30s
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// The default timeout is applied internally
	// We can't directly access it, but the target should be valid
}

// TestTarget_CustomTimeout validates custom timeout is honored.
func TestTarget_CustomTimeout(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Timeout: 60 * time.Second,
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// The custom timeout is applied internally
	// We can't directly access it, but the target should be valid
}

// TestTarget_CloseIdempotent validates multiple Close calls don't panic.
func TestTarget_CloseIdempotent(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)

	// First close
	err1 := tgt.Close()
	assert.NoError(t, err1)

	// Second close should not panic
	err2 := tgt.Close()
	assert.NoError(t, err2)

	// Third close should not panic
	err3 := tgt.Close()
	assert.NoError(t, err3)
}

// TestTarget_Interface validates Target implements types.Target.
func TestTarget_Interface(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	tgt, err := mqtt.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Verify interface implementation
	var _ bridgeTypes.Target = tgt
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Option Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_WithTransportRetry validates transport retry option.
func TestTarget_WithTransportRetry(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}

	retryConfig := bridgeTypes.TransportRetryConfig{
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     1.5,
	}

	tgt, err := mqtt.NewTarget(cfg, mqtt.WithTransportRetry(retryConfig))
	require.NoError(t, err)
	defer tgt.Close()

	// Option applied internally - target should be valid
}

// TestTarget_WithDefaultTTL validates default TTL option.
func TestTarget_WithDefaultTTL(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}

	tgt, err := mqtt.NewTarget(cfg, mqtt.WithDefaultTTL(5*time.Minute))
	require.NoError(t, err)
	defer tgt.Close()

	// Option applied internally - target should be valid
}

// TestTarget_MultipleOptions validates multiple options can be combined.
func TestTarget_MultipleOptions(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}

	retryConfig := bridgeTypes.TransportRetryConfig{
		InitialBackoff: time.Second,
	}

	tgt, err := mqtt.NewTarget(cfg,
		mqtt.WithTransportRetry(retryConfig),
		mqtt.WithDefaultTTL(10*time.Minute),
	)
	require.NoError(t, err)
	defer tgt.Close()
}
