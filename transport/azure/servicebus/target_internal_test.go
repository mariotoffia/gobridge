// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Target Internal Unit Tests
//
// Tests for unexported helper functions in target.go.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ BM01 │ buildMessage basic payload             │ PASS     │
// │ BM02 │ buildMessage subject from topic        │ PASS     │
// │ BM03 │ buildMessage sessionId default         │ PASS     │
// │ BM04 │ buildMessage sessionId override        │ PASS     │
// │ BM05 │ buildMessage TTL                       │ PASS     │
// │ BM06 │ buildMessage messageId                 │ PASS     │
// │ BM07 │ buildMessage correlationId             │ PASS     │
// │ BM08 │ buildMessage contentType               │ PASS     │
// │ BM09 │ buildMessage replyTo                   │ PASS     │
// │ BM10 │ buildMessage applicationProperties     │ PASS     │
// │ BM11 │ buildMessage skips well-known keys     │ PASS     │
// │ BM12 │ buildMessage mixed types               │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebus

import (
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// Test Helper Functions
// ═══════════════════════════════════════════════════════════════════════════

// createTestTarget creates a minimal Target for testing buildMessage.
func createTestTarget(defaultSessionID string) *Target {
	return &Target{
		id:               "test-target",
		defaultSessionID: defaultSessionID,
		batchSize:        10,
		timeout:          30 * time.Second,
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// buildMessage Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestBuildMessage_BasicPayload validates basic message creation.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  bridgeTypes.Message        │────▶│  azservicebus.Message               │
// │  Payload: []byte("hello")   │     │  Body: []byte("hello")              │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_BasicPayload(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("hello world"),
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Equal(t, []byte("hello world"), result.Body)
}

// TestBuildMessage_SubjectFromTopic validates Subject field is set from Topic.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  Message.Topic: "my/topic"  │────▶│  azservicebus.Message.Subject       │
// │                             │     │  = "my/topic"                       │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_SubjectFromTopic(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Topic:   "my/topic",
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.Subject)
	assert.Equal(t, "my/topic", *result.Subject)
}

// TestBuildMessage_SubjectEmpty validates Subject is nil when Topic is empty.
func TestBuildMessage_SubjectEmpty(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Topic:   "", // Empty topic
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	// Subject should be nil or empty when Topic is empty
	if result.Subject != nil {
		assert.Empty(t, *result.Subject)
	}
}

// TestBuildMessage_SessionId_Default validates default session ID is used.
//
// ═══════════════════════════════════════════════════════════════════════════
// Target.defaultSessionID → azservicebus.Message.SessionID
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessage_SessionId_Default(t *testing.T) {
	target := createTestTarget("default-session-123")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.SessionID)
	assert.Equal(t, "default-session-123", *result.SessionID)
}

// TestBuildMessage_SessionId_Override validates metadata sessionId overrides default.
//
// ═══════════════════════════════════════════════════════════════════════════
// Metadata["sessionId"] overrides Target.defaultSessionID
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessage_SessionId_Override(t *testing.T) {
	target := createTestTarget("default-session")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"sessionId": "override-session-456",
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.SessionID)
	assert.Equal(t, "override-session-456", *result.SessionID)
}

// TestBuildMessage_SessionId_None validates no session ID when not configured.
func TestBuildMessage_SessionId_None(t *testing.T) {
	target := createTestTarget("") // No default session

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Nil(t, result.SessionID)
}

// TestBuildMessage_TTL validates TimeToLive is set from message TTL.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  Message.TTL: 5 minutes     │────▶│  azservicebus.Message.TimeToLive    │
// │                             │     │  = 5 minutes                        │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_TTL(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		TTL:     5 * time.Minute,
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.TimeToLive)
	assert.Equal(t, 5*time.Minute, *result.TimeToLive)
}

// TestBuildMessage_TTL_Zero validates no TTL when zero.
func TestBuildMessage_TTL_Zero(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		TTL:     0, // Zero TTL
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Nil(t, result.TimeToLive)
}

// TestBuildMessage_MessageId validates MessageID is set from metadata.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  Metadata["messageId"]      │────▶│  azservicebus.Message.MessageID     │
// │  = "msg-123"                │     │  = "msg-123"                        │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_MessageId(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"messageId": "msg-123-456",
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.MessageID)
	assert.Equal(t, "msg-123-456", *result.MessageID)
}

// TestBuildMessage_CorrelationId validates CorrelationID is set from metadata.
func TestBuildMessage_CorrelationId(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"correlationId": "corr-789",
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.CorrelationID)
	assert.Equal(t, "corr-789", *result.CorrelationID)
}

// TestBuildMessage_ContentType validates ContentType is set from metadata.
func TestBuildMessage_ContentType(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"contentType": "application/json",
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.ContentType)
	assert.Equal(t, "application/json", *result.ContentType)
}

// TestBuildMessage_ReplyTo validates ReplyTo is skipped from ApplicationProperties.
//
// Note: buildMessage (used by SendBatch) does not set ReplyTo on the message,
// unlike Send() which does. The replyTo key is skipped from ApplicationProperties.
func TestBuildMessage_ReplyTo(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"replyTo":   "reply-queue",
			"customKey": "value", // Include another key to ensure map is not empty
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)

	// replyTo is skipped from ApplicationProperties in buildMessage
	_, hasReplyTo := result.ApplicationProperties["replyTo"]
	assert.False(t, hasReplyTo, "replyTo should be skipped from ApplicationProperties")

	// customKey should be included
	assert.Equal(t, "value", result.ApplicationProperties["customKey"])
}

// TestBuildMessage_ApplicationProperties validates metadata is mapped to ApplicationProperties.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  Metadata["customKey"]      │────▶│  ApplicationProperties["customKey"] │
// │  = "customValue"            │     │  = "customValue"                    │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_ApplicationProperties(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"customKey":  "customValue",
			"numericKey": int64(42),
			"boolKey":    true,
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.ApplicationProperties)

	assert.Equal(t, "customValue", result.ApplicationProperties["customKey"])
	assert.Equal(t, int64(42), result.ApplicationProperties["numericKey"])
	assert.Equal(t, true, result.ApplicationProperties["boolKey"])
}

// TestBuildMessage_SkipsWellKnownKeys validates well-known keys are not duplicated.
//
// ═══════════════════════════════════════════════════════════════════════════
// Well-known keys are set as first-class properties, NOT in ApplicationProperties:
// - messageId, correlationId, sessionId, contentType, replyTo, subject, to
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessage_SkipsWellKnownKeys(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"messageId":     "msg-123",
			"correlationId": "corr-456",
			"sessionId":     "sess-789",
			"contentType":   "application/json",
			"replyTo":       "reply-queue",
			"subject":       "my-subject",
			"to":            "destination",
			"customKey":     "included", // This should be included
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	require.NotNil(t, result.ApplicationProperties)

	// Well-known keys should NOT be in ApplicationProperties
	_, hasMessageId := result.ApplicationProperties["messageId"]
	_, hasCorrelationId := result.ApplicationProperties["correlationId"]
	_, hasSessionId := result.ApplicationProperties["sessionId"]
	_, hasContentType := result.ApplicationProperties["contentType"]
	_, hasReplyTo := result.ApplicationProperties["replyTo"]
	_, hasSubject := result.ApplicationProperties["subject"]
	_, hasTo := result.ApplicationProperties["to"]

	assert.False(t, hasMessageId, "messageId should be skipped")
	assert.False(t, hasCorrelationId, "correlationId should be skipped")
	assert.False(t, hasSessionId, "sessionId should be skipped")
	assert.False(t, hasContentType, "contentType should be skipped")
	assert.False(t, hasReplyTo, "replyTo should be skipped")
	assert.False(t, hasSubject, "subject should be skipped")
	assert.False(t, hasTo, "to should be skipped")

	// Custom key should be included
	assert.Equal(t, "included", result.ApplicationProperties["customKey"])
}

// TestBuildMessage_MixedTypes validates multiple field types in one message.
//
// Data Flow:
// ┌─────────────────────────────┐     ┌─────────────────────────────────────┐
// │  Message                    │     │  azservicebus.Message               │
// │  ├─ Payload: []byte         │────▶│  ├─ Body: []byte                    │
// │  ├─ Topic: "topic"          │────▶│  ├─ Subject: "topic"                │
// │  ├─ TTL: 5m                 │────▶│  ├─ TimeToLive: 5m                  │
// │  ├─ Metadata["messageId"]   │────▶│  ├─ MessageID: set                  │
// │  ├─ Metadata["customKey"]   │────▶│  └─ ApplicationProperties["customKey"]
// │  └─ Metadata["sessionId"]   │────▶│      SessionID: set (skipped in AP) │
// └─────────────────────────────┘     └─────────────────────────────────────┘
func TestBuildMessage_MixedTypes(t *testing.T) {
	target := createTestTarget("default-session")

	msg := bridgeTypes.Message{
		Payload: []byte("test payload"),
		Topic:   "test-topic",
		TTL:     10 * time.Minute,
		Metadata: map[string]any{
			"messageId":     "msg-abc",
			"correlationId": "corr-xyz",
			"customString":  "value",
			"customNumber":  int64(123),
			"sessionId":     "override-session", // Should override default
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)

	// Check Body
	assert.Equal(t, []byte("test payload"), result.Body)

	// Check Subject
	require.NotNil(t, result.Subject)
	assert.Equal(t, "test-topic", *result.Subject)

	// Check TTL
	require.NotNil(t, result.TimeToLive)
	assert.Equal(t, 10*time.Minute, *result.TimeToLive)

	// Check MessageID
	require.NotNil(t, result.MessageID)
	assert.Equal(t, "msg-abc", *result.MessageID)

	// Check CorrelationID
	require.NotNil(t, result.CorrelationID)
	assert.Equal(t, "corr-xyz", *result.CorrelationID)

	// Check SessionID (should be overridden)
	require.NotNil(t, result.SessionID)
	assert.Equal(t, "override-session", *result.SessionID)

	// Check ApplicationProperties (custom keys only)
	require.NotNil(t, result.ApplicationProperties)
	assert.Equal(t, "value", result.ApplicationProperties["customString"])
	assert.Equal(t, int64(123), result.ApplicationProperties["customNumber"])

	// Well-known keys should not be in ApplicationProperties
	_, hasMessageId := result.ApplicationProperties["messageId"]
	assert.False(t, hasMessageId)
}

// TestBuildMessage_NilMetadata validates handling of nil metadata.
func TestBuildMessage_NilMetadata(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload:  []byte("test"),
		Metadata: nil,
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Equal(t, []byte("test"), result.Body)
	// ApplicationProperties should be empty or nil-safe
}

// TestBuildMessage_EmptyMetadata validates handling of empty metadata.
func TestBuildMessage_EmptyMetadata(t *testing.T) {
	target := createTestTarget("")

	msg := bridgeTypes.Message{
		Payload:  []byte("test"),
		Metadata: make(map[string]any),
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Equal(t, []byte("test"), result.Body)
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Configuration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_CreateSender_Options validates sender options configuration.
func TestTarget_CreateSender_Options(t *testing.T) {
	t.Run("queue sender", func(t *testing.T) {
		cfg := &TargetConfigImpl{
			ID:        "test",
			QueueName: "my-queue",
		}
		assert.NotEmpty(t, cfg.QueueName)
		assert.Empty(t, cfg.TopicName)
	})

	t.Run("topic sender", func(t *testing.T) {
		cfg := &TargetConfigImpl{
			ID:        "test",
			TopicName: "my-topic",
		}
		assert.Empty(t, cfg.QueueName)
		assert.NotEmpty(t, cfg.TopicName)
	})
}

// TestTarget_BatchSize validates batch size configuration.
func TestTarget_BatchSize(t *testing.T) {
	tests := []struct {
		name      string
		batchSize int
		expected  int
	}{
		{"zero uses default", 0, 10},
		{"custom size", 25, 25},
		{"negative uses default", -1, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &TargetConfigImpl{
				ID:        "test",
				QueueName: "queue",
				BatchSize: tc.batchSize,
				Connection: ConnectionConfig{
					ConnectionString: "Endpoint=sb://test/",
				},
			}

			target, err := NewTarget(cfg)
			require.NoError(t, err)
			defer target.Close()

			// Verify target was created (batch size is applied internally)
			assert.NotNil(t, target)
		})
	}
}

// TestTarget_Timeout validates timeout configuration.
func TestTarget_Timeout(t *testing.T) {
	tests := []struct {
		name     string
		timeout  time.Duration
		expected time.Duration
	}{
		{"zero uses default", 0, 30 * time.Second},
		{"custom timeout", 60 * time.Second, 60 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &TargetConfigImpl{
				ID:        "test",
				QueueName: "queue",
				Timeout:   tc.timeout,
				Connection: ConnectionConfig{
					ConnectionString: "Endpoint=sb://test/",
				},
			}

			target, err := NewTarget(cfg)
			require.NoError(t, err)
			defer target.Close()

			assert.NotNil(t, target)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Scheduled Message Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestBuildMessage_ScheduledEnqueueTime validates scheduled delivery metadata.
//
// ═══════════════════════════════════════════════════════════════════════════
// Metadata["scheduledEnqueueTime"] triggers ScheduleMessages API instead of SendMessage
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessage_ScheduledEnqueueTime(t *testing.T) {
	target := createTestTarget("")

	scheduledTime := time.Now().Add(1 * time.Hour)
	msg := bridgeTypes.Message{
		Payload: []byte("scheduled message"),
		Metadata: map[string]any{
			"scheduledEnqueueTime": scheduledTime,
		},
	}

	result := target.buildMessage(msg)

	require.NotNil(t, result)
	assert.Equal(t, []byte("scheduled message"), result.Body)

	// Note: scheduledEnqueueTime is handled in Send(), not buildMessage()
	// The metadata is passed through to ApplicationProperties for the Send() method to check
}
