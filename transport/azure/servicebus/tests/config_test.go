// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Configuration Unit Tests
//
// Tests for SourceConfigImpl and TargetConfigImpl configuration types.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ C001 │ TransportType constant                 │ PASS     │
// │ C002 │ SourceConfigImpl interface compliance  │ PASS     │
// │ C003 │ SourceConfigImpl getters               │ PASS     │
// │ C004 │ TargetConfigImpl interface compliance  │ PASS     │
// │ C005 │ TargetConfigImpl getters               │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebustests

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/azure/servicebus"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// TransportType Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTransportType_Constant validates the transport type constant.
func TestTransportType_Constant(t *testing.T) {
	assert.Equal(t, types.TransportType("AzureServiceBus"), servicebus.TransportType)
}

// ═══════════════════════════════════════════════════════════════════════════
// SourceConfigImpl Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceConfigImpl_Interface verifies SourceConfigImpl implements types.SourceConfig.
func TestSourceConfigImpl_Interface(t *testing.T) {
	var _ types.SourceConfig = (*servicebus.SourceConfigImpl)(nil)
}

// TestSourceConfigImpl_GetID validates the ID getter.
func TestSourceConfigImpl_GetID(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{ID: "test-source-123"}
	assert.Equal(t, "test-source-123", cfg.GetID())
}

// TestSourceConfigImpl_GetTransportType validates the transport type.
func TestSourceConfigImpl_GetTransportType(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{}
	assert.Equal(t, servicebus.TransportType, cfg.GetTransportType())
}

// TestSourceConfigImpl_GetQoS validates QoS returns at-least-once.
func TestSourceConfigImpl_GetQoS(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{}
	qos := cfg.GetQoS()
	assert.NotNil(t, qos)
	assert.Equal(t, 1, qos.Level, "Service Bus is at-least-once")
}

// TestSourceConfigImpl_GetPrefetch validates prefetch getter.
func TestSourceConfigImpl_GetPrefetch(t *testing.T) {
	tests := []struct {
		name     string
		prefetch int32
		expected int
	}{
		{"zero", 0, 0},
		{"positive", 50, 50},
		{"large", 1000, 1000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &servicebus.SourceConfigImpl{Prefetch: tc.prefetch}
			assert.Equal(t, tc.expected, cfg.GetPrefetch())
		})
	}
}

// TestSourceConfigImpl_GetResources validates resource tags getter.
func TestSourceConfigImpl_GetResources(t *testing.T) {
	resources := []types.Tag{
		{Key: "env", Value: "test"},
		{Key: "app", Value: "myapp"},
	}
	cfg := &servicebus.SourceConfigImpl{Resources: resources}
	assert.Equal(t, resources, cfg.GetResources())
}

// TestSourceConfigImpl_AllowMultipleResourceMatches validates allow multiple flag.
func TestSourceConfigImpl_AllowMultipleResourceMatches(t *testing.T) {
	t.Run("false", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{AllowMultiple: false}
		assert.False(t, cfg.AllowMultipleResourceMatches())
	})

	t.Run("true", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{AllowMultiple: true}
		assert.True(t, cfg.AllowMultipleResourceMatches())
	})
}

// TestSourceConfigImpl_QueueAndTopicConfig validates configuration options.
func TestSourceConfigImpl_QueueAndTopicConfig(t *testing.T) {
	t.Run("queue config", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{
			ID:        "queue-source",
			QueueName: "my-queue",
			Connection: servicebus.ConnectionConfig{
				ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
			},
		}
		assert.Equal(t, "queue-source", cfg.GetID())
		assert.Equal(t, "my-queue", cfg.QueueName)
	})

	t.Run("topic config", func(t *testing.T) {
		cfg := &servicebus.SourceConfigImpl{
			ID:               "topic-source",
			TopicName:        "my-topic",
			SubscriptionName: "my-subscription",
			Connection: servicebus.ConnectionConfig{
				Namespace: "test.servicebus.windows.net",
			},
		}
		assert.Equal(t, "topic-source", cfg.GetID())
		assert.Equal(t, "my-topic", cfg.TopicName)
		assert.Equal(t, "my-subscription", cfg.SubscriptionName)
	})
}

// TestSourceConfigImpl_ReceiveOptions validates receive configuration options.
func TestSourceConfigImpl_ReceiveOptions(t *testing.T) {
	cfg := &servicebus.SourceConfigImpl{
		ID:          "test-source",
		MaxMessages: 50,
		MaxWaitTime: 10 * time.Second,
		ReceiveMode: "ReceiveAndDelete",
		SubQueue:    "deadletter",
		SessionID:   "session-123",
	}

	assert.Equal(t, 50, cfg.MaxMessages)
	assert.Equal(t, 10*time.Second, cfg.MaxWaitTime)
	assert.Equal(t, "ReceiveAndDelete", cfg.ReceiveMode)
	assert.Equal(t, "deadletter", cfg.SubQueue)
	assert.Equal(t, "session-123", cfg.SessionID)
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetConfigImpl Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetConfigImpl_Interface verifies TargetConfigImpl implements types.TargetConfig.
func TestTargetConfigImpl_Interface(t *testing.T) {
	var _ types.TargetConfig = (*servicebus.TargetConfigImpl)(nil)
}

// TestTargetConfigImpl_GetID validates the ID getter.
func TestTargetConfigImpl_GetID(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{ID: "test-target-456"}
	assert.Equal(t, "test-target-456", cfg.GetID())
}

// TestTargetConfigImpl_GetTransportType validates the transport type.
func TestTargetConfigImpl_GetTransportType(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{}
	assert.Equal(t, servicebus.TransportType, cfg.GetTransportType())
}

// TestTargetConfigImpl_GetDefaultQoS validates QoS returns at-least-once.
func TestTargetConfigImpl_GetDefaultQoS(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{}
	qos := cfg.GetDefaultQoS()
	assert.NotNil(t, qos)
	assert.Equal(t, 1, qos.Level, "Service Bus is at-least-once")
}

// TestTargetConfigImpl_GetBatchSize validates batch size getter.
func TestTargetConfigImpl_GetBatchSize(t *testing.T) {
	tests := []struct {
		name      string
		batchSize int
		expected  int
	}{
		{"zero", 0, 0},
		{"one", 1, 1},
		{"ten", 10, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &servicebus.TargetConfigImpl{BatchSize: tc.batchSize}
			assert.Equal(t, tc.expected, cfg.GetBatchSize())
		})
	}
}

// TestTargetConfigImpl_GetTimeout validates timeout getter.
func TestTargetConfigImpl_GetTimeout(t *testing.T) {
	t.Run("zero returns nil", func(t *testing.T) {
		cfg := &servicebus.TargetConfigImpl{Timeout: 0}
		assert.Nil(t, cfg.GetTimeout())
	})

	t.Run("non-zero returns pointer", func(t *testing.T) {
		timeout := 30 * time.Second
		cfg := &servicebus.TargetConfigImpl{Timeout: timeout}
		result := cfg.GetTimeout()
		assert.NotNil(t, result)
		assert.Equal(t, timeout, *result)
	})
}

// TestTargetConfigImpl_GetResources validates resource tags getter.
func TestTargetConfigImpl_GetResources(t *testing.T) {
	resources := []types.Tag{
		{Key: "team", Value: "platform"},
	}
	cfg := &servicebus.TargetConfigImpl{Resources: resources}
	assert.Equal(t, resources, cfg.GetResources())
}

// TestTargetConfigImpl_AllowMultipleResourceMatches validates allow multiple flag.
func TestTargetConfigImpl_AllowMultipleResourceMatches(t *testing.T) {
	t.Run("false", func(t *testing.T) {
		cfg := &servicebus.TargetConfigImpl{AllowMultiple: false}
		assert.False(t, cfg.AllowMultipleResourceMatches())
	})

	t.Run("true", func(t *testing.T) {
		cfg := &servicebus.TargetConfigImpl{AllowMultiple: true}
		assert.True(t, cfg.AllowMultipleResourceMatches())
	})
}

// TestTargetConfigImpl_QueueAndTopicConfig validates configuration options.
func TestTargetConfigImpl_QueueAndTopicConfig(t *testing.T) {
	t.Run("queue config", func(t *testing.T) {
		cfg := &servicebus.TargetConfigImpl{
			ID:        "queue-target",
			QueueName: "my-queue",
			Connection: servicebus.ConnectionConfig{
				ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
			},
		}
		assert.Equal(t, "queue-target", cfg.GetID())
		assert.Equal(t, "my-queue", cfg.QueueName)
	})

	t.Run("topic config", func(t *testing.T) {
		cfg := &servicebus.TargetConfigImpl{
			ID:        "topic-target",
			TopicName: "my-topic",
			Connection: servicebus.ConnectionConfig{
				Namespace: "test.servicebus.windows.net",
			},
		}
		assert.Equal(t, "topic-target", cfg.GetID())
		assert.Equal(t, "my-topic", cfg.TopicName)
	})
}

// TestTargetConfigImpl_SessionConfig validates session configuration.
func TestTargetConfigImpl_SessionConfig(t *testing.T) {
	cfg := &servicebus.TargetConfigImpl{
		ID:               "session-target",
		QueueName:        "session-queue",
		DefaultSessionID: "default-session-123",
	}

	assert.Equal(t, "default-session-123", cfg.DefaultSessionID)
}

// TestConnectionConfig_AuthOptions validates connection authentication options.
func TestConnectionConfig_AuthOptions(t *testing.T) {
	t.Run("connection string auth", func(t *testing.T) {
		cfg := servicebus.ConnectionConfig{
			ConnectionString: "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=key;SharedAccessKey=secret",
		}
		assert.NotEmpty(t, cfg.ConnectionString)
	})

	t.Run("managed identity auth", func(t *testing.T) {
		cfg := servicebus.ConnectionConfig{
			Namespace:          "test.servicebus.windows.net",
			UseManagedIdentity: true,
		}
		assert.Equal(t, "test.servicebus.windows.net", cfg.Namespace)
		assert.True(t, cfg.UseManagedIdentity)
	})

	t.Run("service principal auth", func(t *testing.T) {
		cfg := servicebus.ConnectionConfig{
			Namespace:    "test.servicebus.windows.net",
			TenantID:     "tenant-123",
			ClientID:     "client-456",
			ClientSecret: "secret-789",
		}
		assert.Equal(t, "test.servicebus.windows.net", cfg.Namespace)
		assert.Equal(t, "tenant-123", cfg.TenantID)
		assert.Equal(t, "client-456", cfg.ClientID)
		assert.Equal(t, "secret-789", cfg.ClientSecret)
	})
}
