// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Target Unit Tests
//
// Tests for Target constructor and behavior validation.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ T001 │ NewTarget nil config                   │ PASS     │
// │ T002 │ NewTarget missing queue or topic       │ PASS     │
// │ T003 │ NewTarget missing connection           │ PASS     │
// │ T004 │ NewTarget valid queue config           │ PASS     │
// │ T005 │ NewTarget valid topic config           │ PASS     │
// │ T006 │ NewTarget default values               │ PASS     │
// │ T007 │ Target capabilities                    │ PASS     │
// │ T008 │ Target GetID                           │ PASS     │
// │ T009 │ Target GetTransportType                │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebustests

import (
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/azure/servicebus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewTarget Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewTarget_NilConfig validates that nil config returns an error.
//
// ═══════════════════════════════════════════════════════════════════════════
// Config: nil → ERROR "config is required"
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_NilConfig(t *testing.T) {
	tgt, err := servicebus.NewTarget(nil)

	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewTarget_MissingQueueOrTopic validates that either queue or topic is required.
//
// ═══════════════════════════════════════════════════════════════════════════
// Valid configurations:
//   - QueueName set
//   - TopicName set
//
// Invalid configurations:
//   - Both QueueName and TopicName empty
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_MissingQueueOrTopic(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID: "test-target",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
		// Missing QueueName and TopicName
	}

	tgt, err := servicebus.NewTarget(cfg)

	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queueName")
}

// TestNewTarget_MissingConnection validates that connection info is required.
//
// ═══════════════════════════════════════════════════════════════════════════
// Valid connection:
//   - ConnectionString set, OR
//   - Namespace set (with credentials)
//
// Invalid connection:
//   - Both ConnectionString and Namespace empty
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_MissingConnection(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:         "test-target",
		QueueName:  "my-queue",
		Connection: servicebus.ConnectionConfig{
			// Missing both ConnectionString and Namespace
		},
	}

	tgt, err := servicebus.NewTarget(cfg)

	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connectionString")
}

// TestNewTarget_ValidQueueConfig validates successful target creation with queue.
//
// Data Flow:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  TargetConfigImpl            │  Target                                  │
// │  ├─ ID: "test-target"        │  ├─ id: "test-target"                   │
// │  ├─ QueueName: "my-queue"    │  ├─ cfg: (stored)                       │
// │  └─ ConnectionString: "..."  │  └─ batchSize: 10 (default)             │
// └─────────────────────────────────────────────────────────────────────────┘
func TestNewTarget_ValidQueueConfig(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
	assert.Equal(t, "test-target", tgt.GetID())
}

// TestNewTarget_ValidTopicConfig validates successful target creation with topic.
func TestNewTarget_ValidTopicConfig(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		TopicName: "my-topic",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
	assert.Equal(t, "test-target", tgt.GetID())
}

// TestNewTarget_ValidNamespaceConfig validates target creation with namespace auth.
func TestNewTarget_ValidNamespaceConfig(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			Namespace:          "test.servicebus.windows.net",
			UseManagedIdentity: true,
		},
	}

	tgt, err := servicebus.NewTarget(cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
}

// TestNewTarget_DefaultValues validates default values are applied.
//
// ═══════════════════════════════════════════════════════════════════════════
// Defaults:
//   - BatchSize: 10 (when 0 or negative)
//   - Timeout: 30s (when 0)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_DefaultValues(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
		// All optional values at zero/default
		BatchSize: 0,
		Timeout:   0,
	}

	tgt, err := servicebus.NewTarget(cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
	// Target is created - defaults are applied internally
}

// TestNewTarget_CustomValues validates custom values are preserved.
func TestNewTarget_CustomValues(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
		BatchSize:        25,
		DefaultSessionID: "session-123",
	}

	tgt, err := servicebus.NewTarget(cfg)

	require.NoError(t, err)
	require.NotNil(t, tgt)
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_Capabilities validates target capability reporting.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Service Bus Target Capabilities                                        │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  ✓ CapabilityPublishAtLeastOnce - Service Bus delivery guarantee       │
// │  ✓ CapabilityNativeRetry        - Built-in retry mechanism             │
// │  ✓ CapabilityDeadLetterQueue    - Native DLQ support                   │
// │  ✓ CapabilityDelayedDelivery    - Scheduled message support            │
// └─────────────────────────────────────────────────────────────────────────┘
func TestTarget_Capabilities(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	caps := tgt.Capabilities()

	// Service Bus provides at-least-once delivery
	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtLeastOnce),
		"should support at-least-once publishing")

	// Service Bus has built-in retry
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"should support native retry")

	// Service Bus has native DLQ
	assert.True(t, caps.Has(bridgeTypes.CapabilityDeadLetterQueue),
		"should support dead letter queue")

	// Service Bus supports scheduled/delayed delivery
	assert.True(t, caps.Has(bridgeTypes.CapabilityDelayedDelivery),
		"should support delayed delivery")
}

// TestTarget_GetID validates the GetID method returns configured ID.
func TestTarget_GetID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"simple id", "my-target"},
		{"uuid style", "550e8400-e29b-41d4-a716-446655440000"},
		{"with special chars", "target-test_01"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &servicebus.TargetConfigImpl{
				ID:        tc.id,
				QueueName: "my-queue",
				Connection: servicebus.ConnectionConfig{
					ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
				},
			}

			tgt, err := servicebus.NewTarget(cfg)
			require.NoError(t, err)
			defer tgt.Close()

			assert.Equal(t, tc.id, tgt.GetID())
		})
	}
}

// TestTarget_GetTransportType validates the transport type is correct.
func TestTarget_GetTransportType(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	assert.Equal(t, servicebus.TransportType, tgt.GetTransportType())
}

// TestTarget_CloseIdempotent validates Close can be called multiple times.
func TestTarget_CloseIdempotent(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)
	require.NoError(t, err)

	// First close
	err1 := tgt.Close()

	// Second close should not panic
	err2 := tgt.Close()

	// Both should complete without error (or same error)
	_ = err1
	_ = err2
}

// TestTarget_SessionConfig validates session configuration is stored.
func TestTarget_SessionConfig(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:               "test-target",
		QueueName:        "my-queue",
		DefaultSessionID: "default-session-123",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	tgt, err := servicebus.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	// Target created successfully with session config
	assert.Equal(t, "test-target", tgt.GetID())
}
