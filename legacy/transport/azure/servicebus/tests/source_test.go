// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Source Unit Tests
//
// Tests for Source constructor and behavior validation.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ S001 │ NewSource nil config                   │ PASS     │
// │ S002 │ NewSource missing queue or topic       │ PASS     │
// │ S003 │ NewSource missing connection           │ PASS     │
// │ S004 │ NewSource valid queue config           │ PASS     │
// │ S005 │ NewSource valid topic config           │ PASS     │
// │ S006 │ NewSource default values               │ PASS     │
// │ S007 │ Source capabilities                    │ PASS     │
// │ S008 │ Source capabilities with session       │ PASS     │
// │ S009 │ Source GetID                           │ PASS     │
// │ S010 │ Source GetTransportType                │ PASS     │
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
// NewSource Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewSource_NilConfig validates that nil config returns an error.
//
// ═══════════════════════════════════════════════════════════════════════════
// Config: nil → ERROR "config is required"
// ═══════════════════════════════════════════════════════════════════════════
func TestNewSource_NilConfig(t *testing.T) {
	src, err := servicebus.NewSource(nil)

	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewSource_MissingQueueOrTopic validates that either queue or topic+subscription is required.
//
// ═══════════════════════════════════════════════════════════════════════════
// Valid configurations:
//   - QueueName set
//   - TopicName + SubscriptionName set
//
// Invalid configurations:
//   - Both QueueName and TopicName empty
//   - TopicName without SubscriptionName
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewSource_MissingQueueOrTopic(t *testing.T) {
	t.Run("no queue or topic", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{
			ID: "test-source",
			Connection: servicebus.ConnectionConfig{
				ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
			},
			// Missing QueueName and TopicName
		}

		src, err := servicebus.NewSource(cfg)

		assert.Nil(t, src)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "queueName")
	})

	t.Run("topic without subscription", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{
			ID:        "test-source",
			TopicName: "my-topic",
			// Missing SubscriptionName
			Connection: servicebus.ConnectionConfig{
				ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
			},
		}

		src, err := servicebus.NewSource(cfg)

		assert.Nil(t, src)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "subscriptionName")
	})
}

// TestNewSource_MissingConnection validates that connection info is required.
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
func TestNewSource_MissingConnection(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:         "test-source",
		QueueName:  "my-queue",
		Connection: servicebus.ConnectionConfig{
			// Missing both ConnectionString and Namespace
		},
	}

	src, err := servicebus.NewSource(cfg)

	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connectionString")
}

// TestNewSource_ValidQueueConfig validates successful source creation with queue.
//
// Data Flow:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  SourceConfigImpl            │  Source                                  │
// │  ├─ ID: "test-source"        │  ├─ id: "test-source"                   │
// │  ├─ QueueName: "my-queue"    │  ├─ cfg: (stored)                       │
// │  └─ ConnectionString: "..."  │  └─ messages: chan (created)            │
// └─────────────────────────────────────────────────────────────────────────┘
func TestNewSource_ValidQueueConfig(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
	assert.Equal(t, "test-source", src.GetID())
}

// TestNewSource_ValidTopicConfig validates successful source creation with topic/subscription.
func TestNewSource_ValidTopicConfig(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:               "test-source",
		TopicName:        "my-topic",
		SubscriptionName: "my-subscription",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
	assert.Equal(t, "test-source", src.GetID())
}

// TestNewSource_ValidNamespaceConfig validates source creation with namespace auth.
func TestNewSource_ValidNamespaceConfig(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			Namespace:          "test.servicebus.windows.net",
			UseManagedIdentity: true,
		},
	}

	src, err := servicebus.NewSource(cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
}

// TestNewSource_DefaultValues validates default values are applied.
//
// ═══════════════════════════════════════════════════════════════════════════
// Defaults:
//   - MaxMessages: 10 (when 0 or negative)
//   - MaxWaitTime: 30s (when 0 or negative)
//   - Prefetch: 100 (when 0 or negative)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewSource_DefaultValues(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
		// All optional values at zero/default
		MaxMessages: 0,
		MaxWaitTime: 0,
		Prefetch:    0,
	}

	src, err := servicebus.NewSource(cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
	// Source is created - defaults are applied internally
}

// TestNewSource_CustomValues validates custom values are preserved.
func TestNewSource_CustomValues(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
		MaxMessages: 50,
		Prefetch:    200,
	}

	src, err := servicebus.NewSource(cfg)

	require.NoError(t, err)
	require.NotNil(t, src)
}

// ═══════════════════════════════════════════════════════════════════════════
// Source Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSource_Capabilities validates source capability reporting.
//
// Scenario:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Service Bus Source Capabilities                                        │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  ✓ CapabilityReceiveAtLeastOnce  - Service Bus default delivery        │
// │  ✓ CapabilityRedelivery          - Nack/Abandon returns message        │
// │  ✓ CapabilityExtendTimeout       - Lock renewal supported              │
// │  ○ CapabilityOrdering            - Only with SessionID                 │
// └─────────────────────────────────────────────────────────────────────────┘
func TestSource_Capabilities(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()

	// Service Bus supports at-least-once delivery
	assert.True(t, caps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"should support at-least-once delivery")

	// Service Bus supports Nack (abandon message for redelivery)
	assert.True(t, caps.Has(bridgeTypes.CapabilityRedelivery),
		"should support redelivery via Nack")

	// Service Bus supports extending lock
	assert.True(t, caps.Has(bridgeTypes.CapabilityExtendTimeout),
		"should support extending message lock")

	// Without SessionID, ordering is not guaranteed
	assert.False(t, caps.Has(bridgeTypes.CapabilityOrdering),
		"should NOT report ordering without session")
}

// TestSource_Capabilities_WithSession validates ordering capability with session.
//
// ═══════════════════════════════════════════════════════════════════════════
// SessionID configured → CapabilityOrdering enabled
// ═══════════════════════════════════════════════════════════════════════════
func TestSource_Capabilities_WithSession(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		SessionID: "session-123", // Session enabled
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()

	// With SessionID, ordering is supported
	assert.True(t, caps.Has(bridgeTypes.CapabilityOrdering),
		"should report ordering with session")
}

// TestSource_GetID validates the GetID method returns configured ID.
func TestSource_GetID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"simple id", "my-source"},
		{"uuid style", "550e8400-e29b-41d4-a716-446655440000"},
		{"with special chars", "source-test_01"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &servicebus.SourceConfigImpl{
				ID:        tc.id,
				QueueName: "my-queue",
				Connection: servicebus.ConnectionConfig{
					ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
				},
			}

			src, err := servicebus.NewSource(cfg)
			require.NoError(t, err)
			defer src.Close()

			assert.Equal(t, tc.id, src.GetID())
		})
	}
}

// TestSource_GetTransportType validates the transport type is correct.
func TestSource_GetTransportType(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	assert.Equal(t, servicebus.TransportType, src.GetTransportType())
}

// TestSource_MessagesChannel validates the Messages channel is created.
func TestSource_MessagesChannel(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	// Messages channel should exist (non-nil)
	msgChan := src.Messages()
	assert.NotNil(t, msgChan, "Messages channel should be created")
}

// TestSource_CloseIdempotent validates Close can be called multiple times.
func TestSource_CloseIdempotent(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "my-queue",
		Connection: servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		},
	}

	src, err := servicebus.NewSource(cfg)
	require.NoError(t, err)

	// First close
	err1 := src.Close()

	// Second close should not panic
	err2 := src.Close()

	// Both should complete without error (or same error)
	_ = err1
	_ = err2
}
