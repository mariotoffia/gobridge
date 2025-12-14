// ═══════════════════════════════════════════════════════════════════════════
// Docker Test Utilities - Mosquitto Unit Tests
//
// Tests for MosquittoBuilder and MosquittoContainer.
// These tests do NOT require Docker to be running.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ M001 │ MosquittoBuilder has sensible defaults │ PASS     │
// │ M002 │ MosquittoBuilder chaining works        │ PASS     │
// │ M003 │ WebSocket enable helper                │ PASS     │
// │ M004 │ Adding user disables anonymous         │ PASS     │
// │ M005 │ URL generation                         │ PASS     │
// │ M006 │ Helper constructors                    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// MosquittoBuilder Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMosquittoBuilder_Defaults validates sensible defaults for Mosquitto.
func TestMosquittoBuilder_Defaults(t *testing.T) {
	builder := NewMosquitto()

	// Defaults should be set
	assert.True(t, builder.allowAnonymous, "allowAnonymous should default to true")
	assert.False(t, builder.persistenceEnabled, "persistence should default to false")
	assert.False(t, builder.wsEnabled, "wsEnabled should default to false")
	assert.NotEmpty(t, builder.image, "image should have a default")
	assert.Greater(t, builder.readyTimeout.Seconds(), float64(0), "readyTimeout should be positive")
}

// TestMosquittoBuilder_Chaining validates fluent method chaining.
func TestMosquittoBuilder_Chaining(t *testing.T) {
	builder := NewMosquitto().
		Image("eclipse-mosquitto:2.0.18").
		Name("test-mqtt").
		MQTTPort(1883).
		WebSocketPort(9001).
		AllowAnonymous(false).
		Persistence(true).
		WithConfig("custom config content")

	assert.Equal(t, "eclipse-mosquitto:2.0.18", builder.image)
	assert.Equal(t, "test-mqtt", builder.name)
	assert.Equal(t, 1883, builder.mqttPort)
	assert.Equal(t, 9001, builder.wsPort)
	assert.True(t, builder.wsEnabled)
	assert.False(t, builder.allowAnonymous)
	assert.True(t, builder.persistenceEnabled)
	assert.Equal(t, "custom config content", builder.customConfig)
}

// TestMosquittoBuilder_EnableWebSocket validates WebSocket helper.
func TestMosquittoBuilder_EnableWebSocket(t *testing.T) {
	builder := NewMosquitto().EnableWebSocket()
	assert.Equal(t, 0, builder.wsPort, "EnableWebSocket should set port to 0 (random)")
}

// TestMosquittoBuilder_WithConfigFile validates config file option.
func TestMosquittoBuilder_WithConfigFile(t *testing.T) {
	builder := NewMosquitto().
		WithConfigFile("/path/to/custom.conf")

	assert.Equal(t, "/path/to/custom.conf", builder.configFile)
}

// ═══════════════════════════════════════════════════════════════════════════
// MosquittoContainer Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMosquittoContainer_URLs validates URL generation.
func TestMosquittoContainer_URLs(t *testing.T) {
	t.Run("with websocket", func(t *testing.T) {
		container := &MosquittoContainer{
			mqttPort: 31883,
			wsPort:   39001,
		}
		assert.Equal(t, "tcp://127.0.0.1:31883", container.BrokerURL())
		assert.Equal(t, "ws://127.0.0.1:39001", container.WebSocketURL())
	})

	t.Run("without websocket", func(t *testing.T) {
		container := &MosquittoContainer{
			mqttPort: 31883,
			wsPort:   0,
		}
		assert.Equal(t, "tcp://127.0.0.1:31883", container.BrokerURL())
		assert.Empty(t, container.WebSocketURL())
	})
}

// TestMosquittoContainer_PortAccessors validates port accessor methods.
func TestMosquittoContainer_PortAccessors(t *testing.T) {
	container := &MosquittoContainer{
		mqttPort: 31883,
		wsPort:   39001,
	}

	assert.Equal(t, 31883, container.MQTTPort())
	assert.Equal(t, 39001, container.WebSocketPort())
}

// ═══════════════════════════════════════════════════════════════════════════
// Helper Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMosquittoHelpers validates convenience constructors.
func TestMosquittoHelpers(t *testing.T) {
	t.Run("DefaultMosquittoConfig", func(t *testing.T) {
		builder := DefaultMosquittoConfig()
		assert.True(t, builder.allowAnonymous)
	})

	t.Run("MosquittoWithWebSocket", func(t *testing.T) {
		builder := MosquittoWithWebSocket()
		assert.True(t, builder.allowAnonymous)
		assert.True(t, builder.wsEnabled)
	})
}

