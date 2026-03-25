// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Factory Unit Tests
//
// Tests for SQS SourceFactory and TargetFactory implementations.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ F001 │ SourceFactory interface compliance     │ PASS     │
// │ F002 │ SourceFactory SupportedTransports      │ PASS     │
// │ F003 │ SourceFactory CreateSource invalid cfg │ PASS     │
// │ F004 │ SourceFactory CreateSource valid       │ PASS     │
// │ F005 │ TargetFactory interface compliance     │ PASS     │
// │ F006 │ TargetFactory SupportedTransports      │ PASS     │
// │ F007 │ TargetFactory CreateTarget invalid cfg │ PASS     │
// │ F008 │ TargetFactory CreateTarget valid       │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqstests

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// SourceFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceFactory_Interface verifies SourceFactory implements types.SourceFactory.
func TestSourceFactory_Interface(t *testing.T) {
	var _ types.SourceFactory = (*sqs.SourceFactory)(nil)
}

// TestSourceFactory_SupportedTransports validates supported transports.
func TestSourceFactory_SupportedTransports(t *testing.T) {
	factory := sqs.NewSourceFactory()
	transports := factory.SupportedTransports()

	assert.Len(t, transports, 1)
	assert.Contains(t, transports, sqs.TransportType)
}

// TestSourceFactory_CreateSource_InvalidConfig validates wrong config type error.
func TestSourceFactory_CreateSource_InvalidConfig(t *testing.T) {
	factory := sqs.NewSourceFactory()
	ctx := context.Background()

	// Use a different config type (mismatched)
	wrongConfig := &mockSourceConfig{id: "test"}

	src, err := factory.CreateSource(ctx, wrongConfig)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// TestSourceFactory_CreateSource_Valid validates successful source creation.
func TestSourceFactory_CreateSource_Valid(t *testing.T) {
	factory := sqs.NewSourceFactory()
	ctx := context.Background()

	cfg := &sqs.SourceConfigImpl{
		ID:       "test-source",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}

	src, err := factory.CreateSource(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()

	assert.Equal(t, "test-source", src.GetID())
	assert.Equal(t, sqs.TransportType, src.GetTransportType())
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetFactory Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetFactory_Interface verifies TargetFactory implements types.TargetFactory.
func TestTargetFactory_Interface(t *testing.T) {
	var _ types.TargetFactory = (*sqs.TargetFactory)(nil)
}

// TestTargetFactory_SupportedTransports validates supported transports.
func TestTargetFactory_SupportedTransports(t *testing.T) {
	factory := sqs.NewTargetFactory()
	transports := factory.SupportedTransports()

	assert.Len(t, transports, 1)
	assert.Contains(t, transports, sqs.TransportType)
}

// TestTargetFactory_CreateTarget_InvalidConfig validates wrong config type error.
func TestTargetFactory_CreateTarget_InvalidConfig(t *testing.T) {
	factory := sqs.NewTargetFactory()
	ctx := context.Background()

	// Use a different config type (mismatched)
	wrongConfig := &mockTargetConfig{id: "test"}

	tgt, err := factory.CreateTarget(ctx, wrongConfig)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

// TestTargetFactory_CreateTarget_Valid validates successful target creation.
func TestTargetFactory_CreateTarget_Valid(t *testing.T) {
	factory := sqs.NewTargetFactory()
	ctx := context.Background()

	cfg := &sqs.TargetConfigImpl{
		ID:       "test-target",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}

	tgt, err := factory.CreateTarget(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()

	assert.Equal(t, "test-target", tgt.GetID())
	assert.Equal(t, sqs.TransportType, tgt.GetTransportType())
}

// ═══════════════════════════════════════════════════════════════════════════
// Mock Configs for Testing
// ═══════════════════════════════════════════════════════════════════════════

// mockSourceConfig is a mock that implements types.SourceConfig but is not sqs.SourceConfigImpl.
type mockSourceConfig struct {
	id string
}

func (m *mockSourceConfig) GetID() string                         { return m.id }
func (m *mockSourceConfig) GetTransportType() types.TransportType { return "MOCK" }
func (m *mockSourceConfig) GetQoS() *types.QosLevel               { return nil }
func (m *mockSourceConfig) GetPrefetch() int                      { return 0 }
func (m *mockSourceConfig) GetResources() []types.Tag             { return nil }
func (m *mockSourceConfig) AllowMultipleResourceMatches() bool    { return false }

// mockTargetConfig is a mock that implements types.TargetConfig but is not sqs.TargetConfigImpl.
type mockTargetConfig struct {
	id string
}

func (m *mockTargetConfig) GetID() string                         { return m.id }
func (m *mockTargetConfig) GetTransportType() types.TransportType { return "MOCK" }
func (m *mockTargetConfig) GetDefaultQoS() *types.QosLevel        { return nil }
func (m *mockTargetConfig) GetBatchSize() int                     { return 0 }
func (m *mockTargetConfig) GetTimeout() *time.Duration            { return nil }
func (m *mockTargetConfig) GetResources() []types.Tag             { return nil }
func (m *mockTargetConfig) AllowMultipleResourceMatches() bool    { return false }
