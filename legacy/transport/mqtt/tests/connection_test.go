// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Connection Unit Tests
//
// Tests for MQTTConnection constructor validation and behavior.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ CN001│ NewConnection nil config error         │ PASS     │
// │ CN002│ NewConnection no broker error          │ PASS     │
// │ CN003│ NewConnection valid config success     │ PASS     │
// │ CN004│ Connection GetID correct               │ PASS     │
// │ CN005│ Connection GetTransportType MQTT       │ PASS     │
// │ CN006│ Connection Capabilities                │ PASS     │
// │ CN007│ CreateSource before Start error        │ PASS     │
// │ CN008│ CreateTarget before Start error        │ PASS     │
// │ CN009│ Connection IsRunning state             │ PASS     │
// │ CN010│ Connection IsDraining state            │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"context"
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewConnection Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewConnection_NilConfig validates nil config returns error.
func TestNewConnection_NilConfig(t *testing.T) {
	conn, err := mqtt.NewConnection(nil)
	assert.Nil(t, conn)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewConnection_NoBroker validates missing broker returns error.
func TestNewConnection_NoBroker(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		// Connection.BrokerURL is empty
	}
	conn, err := mqtt.NewConnection(cfg)
	assert.Nil(t, conn)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "broker URL is required")
}

// TestNewConnection_ValidConfig validates valid config succeeds.
func TestNewConnection_ValidConfig(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)
	require.NotNil(t, conn)

	// Connection is created but not started
	assert.False(t, conn.IsRunning())
}

// ═══════════════════════════════════════════════════════════════════════════
// Connection Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestConnection_GetID validates ID getter returns configured value.
func TestConnection_GetID(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "my-connection-id",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	assert.Equal(t, "my-connection-id", conn.GetID())
}

// TestConnection_GetTransportType validates transport type returns MQTT.
func TestConnection_GetTransportType(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	assert.Equal(t, mqtt.TransportType, conn.GetTransportType())
}

// TestConnection_Capabilities validates reported capabilities.
//
// MQTTConnection supports all QoS levels and native retry for QoS 1/2.
func TestConnection_Capabilities(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	caps := conn.Capabilities()

	// Should have entry for "*" (all topics)
	assert.Contains(t, caps, "*")

	topicCaps := caps["*"]

	// Publish capabilities
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityPublishAtMostOnce),
		"should have PublishAtMostOnce")
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityPublishAtLeastOnce),
		"should have PublishAtLeastOnce")
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityPublishExactOnce),
		"should have PublishExactOnce")

	// Receive capabilities
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityReceiveAtMostOnce),
		"should have ReceiveAtMostOnce")
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"should have ReceiveAtLeastOnce")
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityReceiveExactOnce),
		"should have ReceiveExactOnce")

	// Native retry
	assert.True(t, topicCaps.Has(bridgeTypes.CapabilityNativeRetry),
		"should have NativeRetry")
}

// TestConnection_Capabilities_SpecificTopics validates topic-specific capabilities.
func TestConnection_Capabilities_SpecificTopics(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	caps := conn.Capabilities("topic/one", "topic/two")

	// Should have entries for specific topics
	assert.Contains(t, caps, "topic/one")
	assert.Contains(t, caps, "topic/two")
	assert.NotContains(t, caps, "*")
}

// TestConnection_CreateSource_BeforeStart validates CreateSource fails before Start.
func TestConnection_CreateSource_BeforeStart(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	// Try to create source before Start
	srcCfg := &mqtt.SourceConfigImpl{
		ID:     "test-source",
		Topics: []string{"test/topic"},
	}
	src, err := conn.CreateSource(context.Background(), srcCfg)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

// TestConnection_CreateTarget_BeforeStart validates CreateTarget fails before Start.
func TestConnection_CreateTarget_BeforeStart(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	// Try to create target before Start
	tgtCfg := &mqtt.TargetConfigImpl{
		ID:           "test-target",
		DefaultTopic: "test/topic",
	}
	tgt, err := conn.CreateTarget(context.Background(), tgtCfg)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

// TestConnection_IsRunning validates IsRunning state tracking.
func TestConnection_IsRunning(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	// Before Start
	assert.False(t, conn.IsRunning(), "should not be running before Start")
}

// TestConnection_IsDraining validates IsDraining state tracking.
func TestConnection_IsDraining(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	// Before any operations
	assert.False(t, conn.IsDraining(), "should not be draining initially")
}

// TestConnection_Interface validates Connection implements required interfaces.
func TestConnection_Interface(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	// Verify interface implementations
	var _ bridgeTypes.Connection = conn
	var _ bridgeTypes.SourceProvider = conn
	var _ bridgeTypes.TargetProvider = conn

	// Verify providers return the connection
	assert.Equal(t, conn, conn.SourceProvider())
	assert.Equal(t, conn, conn.TargetProvider())
}

// TestConnection_LifecycleCoordinator validates coordinator is available.
func TestConnection_LifecycleCoordinator(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	assert.NotNil(t, coordinator, "should have lifecycle coordinator")
}
