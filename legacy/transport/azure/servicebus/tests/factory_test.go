// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Factory Unit Tests
//
// Tests for SourceFactory and TargetFactory implementations.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ F001 │ SourceFactory interface compliance     │ PASS     │
// │ F002 │ SourceFactory SupportedTransports      │ PASS     │
// │ F003 │ SourceFactory CreateSource invalid     │ PASS     │
// │ F004 │ TargetFactory interface compliance     │ PASS     │
// │ F005 │ TargetFactory SupportedTransports      │ PASS     │
// │ F006 │ TargetFactory CreateTarget invalid     │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebustests

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/azure/servicebus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// SourceFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceFactory_Interface verifies SourceFactory implements types.SourceFactory.
func TestSourceFactory_Interface(t *testing.T) {
	var _ types.SourceFactory = (*servicebus.SourceFactory)(nil)
}

// TestNewSourceFactory validates factory constructor.
func TestNewSourceFactory(t *testing.T) {
	factory := servicebus.NewSourceFactory()
	assert.NotNil(t, factory)
}

// TestSourceFactory_SupportedTransports validates supported transports.
//
// ═══════════════════════════════════════════════════════════════════════════
// SourceFactory supports: [AzureServiceBus]
// ═══════════════════════════════════════════════════════════════════════════
func TestSourceFactory_SupportedTransports(t *testing.T) {
	factory := servicebus.NewSourceFactory()

	transports := factory.SupportedTransports()

	require.Len(t, transports, 1)
	assert.Equal(t, servicebus.TransportType, transports[0])
}

// TestSourceFactory_CreateSource_InvalidConfigType validates error on wrong config type.
//
// Data Flow:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  CreateSource(ctx, wrongType)                                           │
// │      │                                                                  │
// │      ▼                                                                  │
// │  Type assertion fails → ERROR "invalid config type"                     │
// └─────────────────────────────────────────────────────────────────────────┘
func TestSourceFactory_CreateSource_InvalidConfigType(t *testing.T) {
	factory := servicebus.NewSourceFactory()
	ctx := context.Background()

	// Use a mock config type that implements SourceConfig but isn't SourceConfigImpl
	wrongConfig := &mockSourceConfig{}

	src, err := factory.CreateSource(ctx, wrongConfig)

	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// mockSourceConfig implements types.SourceConfig for testing wrong config types
type mockSourceConfig struct{}

func (m *mockSourceConfig) GetID() string                         { return "mock" }
func (m *mockSourceConfig) GetTransportType() types.TransportType { return "mock" }
func (m *mockSourceConfig) GetQoS() *types.QosLevel               { return nil }
func (m *mockSourceConfig) GetPrefetch() int                      { return 0 }
func (m *mockSourceConfig) GetResources() []types.Tag             { return nil }
func (m *mockSourceConfig) AllowMultipleResourceMatches() bool    { return false }

// TestSourceFactory_CreateSource_ValidConfig validates successful source creation.
func TestSourceFactory_CreateSource_ValidConfig(t *testing.T) {
	factory := servicebus.NewSourceFactory()
	ctx := context.Background()

	cfg := &servicebus.SourceConfigImpl{
		ID:        "factory-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := factory.CreateSource(ctx, cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()

	assert.Equal(t, "factory-source", src.GetID())
	assert.Equal(t, servicebus.TransportType, src.GetTransportType())
}

// TestSourceFactory_CreateSource_InvalidConfig validates error on invalid config.
func TestSourceFactory_CreateSource_InvalidConfig(t *testing.T) {
	factory := servicebus.NewSourceFactory()
	ctx := context.Background()

	// Config missing required fields
	cfg := &servicebus.SourceConfigImpl{
		ID: "invalid-source",
		// Missing QueueName/TopicName and Connection
	}

	src, err := factory.CreateSource(ctx, cfg)

	assert.Nil(t, src)
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetFactory_Interface verifies TargetFactory implements types.TargetFactory.
func TestTargetFactory_Interface(t *testing.T) {
	var _ types.TargetFactory = (*servicebus.TargetFactory)(nil)
}

// TestNewTargetFactory validates factory constructor.
func TestNewTargetFactory(t *testing.T) {
	factory := servicebus.NewTargetFactory()
	assert.NotNil(t, factory)
}

// TestTargetFactory_SupportedTransports validates supported transports.
//
// ═══════════════════════════════════════════════════════════════════════════
// TargetFactory supports: [AzureServiceBus]
// ═══════════════════════════════════════════════════════════════════════════
func TestTargetFactory_SupportedTransports(t *testing.T) {
	factory := servicebus.NewTargetFactory()

	transports := factory.SupportedTransports()

	require.Len(t, transports, 1)
	assert.Equal(t, servicebus.TransportType, transports[0])
}

// TestTargetFactory_CreateTarget_InvalidConfigType validates error on wrong config type.
//
// Data Flow:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  CreateTarget(ctx, wrongType)                                           │
// │      │                                                                  │
// │      ▼                                                                  │
// │  Type assertion fails → ERROR "invalid config type"                     │
// └─────────────────────────────────────────────────────────────────────────┘
func TestTargetFactory_CreateTarget_InvalidConfigType(t *testing.T) {
	factory := servicebus.NewTargetFactory()
	ctx := context.Background()

	// Use a mock config type that implements TargetConfig but isn't TargetConfigImpl
	wrongConfig := &mockTargetConfig{}

	tgt, err := factory.CreateTarget(ctx, wrongConfig)

	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// mockTargetConfig implements types.TargetConfig for testing wrong config types
type mockTargetConfig struct{}

func (m *mockTargetConfig) GetID() string                         { return "mock" }
func (m *mockTargetConfig) GetTransportType() types.TransportType { return "mock" }
func (m *mockTargetConfig) GetDefaultQoS() *types.QosLevel        { return nil }
func (m *mockTargetConfig) GetBatchSize() int                     { return 0 }
func (m *mockTargetConfig) GetTimeout() *time.Duration            { return nil }
func (m *mockTargetConfig) GetResources() []types.Tag             { return nil }
func (m *mockTargetConfig) AllowMultipleResourceMatches() bool    { return false }

// TestTargetFactory_CreateTarget_ValidConfig validates successful target creation.
func TestTargetFactory_CreateTarget_ValidConfig(t *testing.T) {
	factory := servicebus.NewTargetFactory()
	ctx := context.Background()

	cfg := &servicebus.TargetConfigImpl{
		ID:        "factory-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := factory.CreateTarget(ctx, cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()

	assert.Equal(t, "factory-target", tgt.GetID())
	assert.Equal(t, servicebus.TransportType, tgt.GetTransportType())
}

// TestTargetFactory_CreateTarget_InvalidConfig validates error on invalid config.
func TestTargetFactory_CreateTarget_InvalidConfig(t *testing.T) {
	factory := servicebus.NewTargetFactory()
	ctx := context.Background()

	// Config missing required fields
	cfg := &servicebus.TargetConfigImpl{
		ID: "invalid-target",
		// Missing QueueName/TopicName and Connection
	}

	tgt, err := factory.CreateTarget(ctx, cfg)

	assert.Nil(t, tgt)
	assert.Error(t, err)
}
