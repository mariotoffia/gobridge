// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Factory Unit Tests
//
// Tests for SourceFactory and TargetFactory implementations.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ F001 │ SourceFactory SupportedTransports      │ PASS     │
// │ F002 │ SourceFactory CreateSource wrong type  │ PASS     │
// │ F003 │ TargetFactory SupportedTransports      │ PASS     │
// │ F004 │ TargetFactory CreateTarget wrong type  │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"context"
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// SourceFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceFactory_Interface validates SourceFactory implements interface.
func TestSourceFactory_Interface(t *testing.T) {
	factory := mqtt.NewSourceFactory()
	var _ bridgeTypes.SourceFactory = factory
}

// TestSourceFactory_SupportedTransports validates MQTT is supported.
func TestSourceFactory_SupportedTransports(t *testing.T) {
	factory := mqtt.NewSourceFactory()
	transports := factory.SupportedTransports()

	require.Len(t, transports, 1)
	assert.Equal(t, mqtt.TransportType, transports[0])
}

// TestSourceFactory_CreateSource_ValidConfig validates source creation.
func TestSourceFactory_CreateSource_ValidConfig(t *testing.T) {
	factory := mqtt.NewSourceFactory()
	cfg := &mqtt.SourceConfigImpl{
		ID: "test-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
	}

	src, err := factory.CreateSource(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()

	assert.Equal(t, "test-source", src.GetID())
}

// TestSourceFactory_CreateSource_WrongType validates wrong config type error.
func TestSourceFactory_CreateSource_WrongType(t *testing.T) {
	factory := mqtt.NewSourceFactory()

	// Use a mock config that implements SourceConfig but isn't MQTT
	wrongCfg := &mockSourceConfig{id: "wrong-type"}

	src, err := factory.CreateSource(context.Background(), wrongCfg)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetFactory_Interface validates TargetFactory implements interface.
func TestTargetFactory_Interface(t *testing.T) {
	factory := mqtt.NewTargetFactory()
	var _ bridgeTypes.TargetFactory = factory
}

// TestTargetFactory_SupportedTransports validates MQTT is supported.
func TestTargetFactory_SupportedTransports(t *testing.T) {
	factory := mqtt.NewTargetFactory()
	transports := factory.SupportedTransports()

	require.Len(t, transports, 1)
	assert.Equal(t, mqtt.TransportType, transports[0])
}

// TestTargetFactory_CreateTarget_ValidConfig validates target creation.
func TestTargetFactory_CreateTarget_ValidConfig(t *testing.T) {
	factory := mqtt.NewTargetFactory()
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		DefaultTopic: "test/topic",
	}

	tgt, err := factory.CreateTarget(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()

	assert.Equal(t, "test-target", tgt.GetID())
}

// TestTargetFactory_CreateTarget_WrongType validates wrong config type error.
func TestTargetFactory_CreateTarget_WrongType(t *testing.T) {
	factory := mqtt.NewTargetFactory()

	// Use a mock config that implements TargetConfig but isn't MQTT
	wrongCfg := &mockTargetConfig{id: "wrong-type"}

	tgt, err := factory.CreateTarget(context.Background(), wrongCfg)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// ═══════════════════════════════════════════════════════════════════════════
// Mock Config Types for Testing
// ═══════════════════════════════════════════════════════════════════════════

// mockSourceConfig is a non-MQTT source config for testing.
type mockSourceConfig struct {
	id string
}

func (c *mockSourceConfig) GetID() string                               { return c.id }
func (c *mockSourceConfig) GetTransportType() bridgeTypes.TransportType { return "MOCK" }
func (c *mockSourceConfig) GetQoS() *bridgeTypes.QosLevel               { return nil }
func (c *mockSourceConfig) GetPrefetch() int                            { return 0 }
func (c *mockSourceConfig) GetResources() []bridgeTypes.Tag             { return nil }
func (c *mockSourceConfig) AllowMultipleResourceMatches() bool          { return false }

// mockTargetConfig is a non-MQTT target config for testing.
type mockTargetConfig struct {
	id string
}

func (c *mockTargetConfig) GetID() string                               { return c.id }
func (c *mockTargetConfig) GetTransportType() bridgeTypes.TransportType { return "MOCK" }
func (c *mockTargetConfig) GetDefaultQoS() *bridgeTypes.QosLevel        { return nil }
func (c *mockTargetConfig) GetBatchSize() int                           { return 0 }
func (c *mockTargetConfig) GetTimeout() *time.Duration                  { return nil }
func (c *mockTargetConfig) GetResources() []bridgeTypes.Tag             { return nil }
func (c *mockTargetConfig) AllowMultipleResourceMatches() bool          { return false }
