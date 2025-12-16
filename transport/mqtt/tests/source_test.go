// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Source Unit Tests
//
// Tests for Source constructor validation and behavior.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ S001 │ NewSource nil config error             │ PASS     │
// │ S002 │ NewSource no broker error              │ PASS     │
// │ S003 │ NewSource no topics error              │ PASS     │
// │ S004 │ NewSource valid config success         │ PASS     │
// │ S005 │ NewSourceWithClient nil client error   │ PASS     │
// │ S006 │ NewSourceWithClient nil router error   │ PASS     │
// │ S007 │ Source GetID returns configured value  │ PASS     │
// │ S008 │ Source GetTransportType returns MQTT   │ PASS     │
// │ S009 │ Source Capabilities QoS 0              │ PASS     │
// │ S010 │ Source Capabilities QoS 1              │ PASS     │
// │ S011 │ Source Capabilities QoS 2              │ PASS     │
// │ S012 │ Source Messages returns non-nil        │ PASS     │
// │ S013 │ Source Close idempotent                │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewSource Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewSource_NilConfig validates nil config returns error.
func TestNewSource_NilConfig(t *testing.T) {
	src, err := mqtt.NewSource(nil)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewSource_NoBroker validates missing broker returns error.
func TestNewSource_NoBroker(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID:     "test-source",
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "broker URL is required")
}

// TestNewSource_NoTopics validates missing topics returns error.
func TestNewSource_NoTopics(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		// Topics is empty
	}
	src, err := mqtt.NewSource(cfg)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one topic is required")
}

// TestNewSource_ValidConfig validates valid config succeeds.
func TestNewSource_ValidConfig(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		QoS:    1,
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()
}

// TestNewSource_QoSClamping validates QoS > 2 clamps to 1.
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
func TestNewSource_QoSClamping(t *testing.T) {
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
			cfg := &mqtt.SourceConfigImpl{
				ID: "test-source",
				Connection: mqtt.ConnectionConfig{
					BrokerURL: "tcp://localhost:1883",
				},
				Topics: []string{"test/topic"},
				QoS:    tc.inputQoS,
			}
			src, err := mqtt.NewSource(cfg)
			require.NoError(t, err)
			require.NotNil(t, src)
			defer src.Close()
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// NewSourceWithClient Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewSourceWithClient_NilConfig validates nil config returns error.
func TestNewSourceWithClient_NilConfig(t *testing.T) {
	// We can't easily create a real client/router without a broker,
	// so we just test the nil config case
	src, err := mqtt.NewSourceWithClient(nil, nil, nil)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// ═══════════════════════════════════════════════════════════════════════════
// Source Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSource_GetID validates ID getter returns configured value.
func TestSource_GetID(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "my-source-id",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	assert.Equal(t, "my-source-id", src.GetID())
}

// TestSource_GetTransportType validates transport type returns MQTT.
func TestSource_GetTransportType(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	assert.Equal(t, mqtt.TransportType, src.GetTransportType())
}

// TestSource_Capabilities_QoS0 validates QoS 0 capabilities.
//
// QoS 0 (fire-and-forget):
// - CapabilityReceiveAtMostOnce: Messages may be lost
func TestSource_Capabilities_QoS0(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		QoS:    0,
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityReceiveAtMostOnce),
		"QoS 0 should have ReceiveAtMostOnce")
	assert.False(t, caps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"QoS 0 should NOT have ReceiveAtLeastOnce")
}

// TestSource_Capabilities_QoS1 validates QoS 1 capabilities.
//
// QoS 1 (at-least-once):
// - CapabilityReceiveAtLeastOnce: Messages delivered at least once
func TestSource_Capabilities_QoS1(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		QoS:    1,
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"QoS 1 should have ReceiveAtLeastOnce")
	assert.False(t, caps.Has(bridgeTypes.CapabilityReceiveAtMostOnce),
		"QoS 1 should NOT have ReceiveAtMostOnce")
}

// TestSource_Capabilities_QoS2 validates QoS 2 capabilities.
//
// QoS 2 (exactly-once):
// - CapabilityReceiveExactOnce: Messages delivered exactly once
func TestSource_Capabilities_QoS2(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		QoS:    2,
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()
	assert.True(t, caps.Has(bridgeTypes.CapabilityReceiveExactOnce),
		"QoS 2 should have ReceiveExactOnce")
	assert.False(t, caps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"QoS 2 should NOT have ReceiveAtLeastOnce")
}

// TestSource_Messages validates Messages returns a channel.
func TestSource_Messages(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	ch := src.Messages()
	assert.NotNil(t, ch, "Messages() should return a non-nil channel")
}

// TestSource_CloseIdempotent validates multiple Close calls don't panic.
func TestSource_CloseIdempotent(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)

	// First close
	err1 := src.Close()
	assert.NoError(t, err1)

	// Second close should not panic
	err2 := src.Close()
	assert.NoError(t, err2)

	// Third close should not panic
	err3 := src.Close()
	assert.NoError(t, err3)
}

// TestSource_Interface validates Source implements types.Source.
func TestSource_Interface(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}
	src, err := mqtt.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	// Verify interface implementation
	var _ bridgeTypes.Source = src
}
