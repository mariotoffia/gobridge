// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Source Internal Unit Tests
//
// # Tests for unexported methods in source.go
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ SI001│ handleMessage when not running         │ PASS     │
// │ SI002│ handleMessage extracts properties      │ PASS     │
// │ SI003│ handleMessage handles full channel     │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtt

import (
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// handleMessage Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSource_HandleMessage_NotRunning validates messages are dropped when not running.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Source.running = false
//	handleMessage() called → message dropped (no panic)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSource_HandleMessage_NotRunning(t *testing.T) {
	src := &Source{
		id:       "test-source",
		messages: make(chan *types.SourceMessage, 10),
		qos:      1,
	}
	// running is false by default (zero value)

	msg := &paho.Publish{
		Topic:   "test/topic",
		Payload: []byte("test payload"),
		QoS:     1,
	}

	// Should not panic
	src.handleMessage(msg)

	// Channel should be empty
	select {
	case <-src.messages:
		t.Fatal("message should not be sent when not running")
	default:
		// Expected - no message
	}
}

// TestSource_HandleMessage_ExtractsProperties validates MQTT v5 properties are extracted.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	MQTT v5 message with:
//	- CorrelationData
//	- ResponseTopic
//	- ContentType
//	- MessageExpiry
//	- UserProperties
//
//	All should be extracted into Message.Metadata
//
// ───────────────────────────────────────────────────────────────────────────
func TestSource_HandleMessage_ExtractsProperties(t *testing.T) {
	src := &Source{
		id:       "test-source",
		messages: make(chan *types.SourceMessage, 10),
		qos:      1,
	}
	src.running.Store(true)

	expiry := uint32(60)
	msg := &paho.Publish{
		Topic:   "test/topic",
		Payload: []byte("test payload"),
		QoS:     1,
		Properties: &paho.PublishProperties{
			CorrelationData: []byte("corr-123"),
			ResponseTopic:   "response/topic",
			ContentType:     "application/json",
			MessageExpiry:   &expiry,
			User: []paho.UserProperty{
				{Key: "custom-key", Value: "custom-value"},
			},
		},
	}

	src.handleMessage(msg)

	// Should receive message
	select {
	case received := <-src.messages:
		require.NotNil(t, received)
		assert.Equal(t, "test/topic", received.Message.Topic)
		assert.Equal(t, []byte("test payload"), received.Message.Payload)
		assert.Equal(t, 1, received.Message.Qos.Level)

		// Check metadata
		assert.Equal(t, []byte("corr-123"), received.Message.Metadata["correlationData"])
		assert.Equal(t, "response/topic", received.Message.Metadata["responseTopic"])
		assert.Equal(t, "application/json", received.Message.Metadata["contentType"])

		// Check TTL from MessageExpiry
		assert.Equal(t, 60*time.Second, received.Message.TTL)

		// Check user properties
		userProps, ok := received.Message.Metadata["userProperties"].(map[string]string)
		require.True(t, ok)
		assert.Equal(t, "custom-value", userProps["custom-key"])

	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

// TestSource_HandleMessage_ChannelFull validates full channel is handled gracefully.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Channel buffer is full
//	handleMessage() called → message dropped (no block/panic)
//
// ───────────────────────────────────────────────────────────────────────────
func TestSource_HandleMessage_ChannelFull(t *testing.T) {
	// Create source with tiny buffer
	src := &Source{
		id:       "test-source",
		messages: make(chan *types.SourceMessage, 1),
		qos:      1,
	}
	src.running.Store(true)

	msg := &paho.Publish{
		Topic:   "test/topic",
		Payload: []byte("test payload"),
		QoS:     1,
	}

	// Fill the channel
	src.handleMessage(msg)

	// This should not block - message is dropped
	done := make(chan struct{})
	go func() {
		src.handleMessage(msg)
		close(done)
	}()

	select {
	case <-done:
		// Expected - returned without blocking
	case <-time.After(time.Second):
		t.Fatal("handleMessage blocked on full channel")
	}
}

// TestSource_GetTopics validates getTopics returns configured topics.
func TestSource_GetTopics(t *testing.T) {
	src := &Source{
		id:     "test-source",
		topics: []string{"topic/one", "topic/two"},
	}

	topics := src.getTopics()
	assert.Equal(t, []string{"topic/one", "topic/two"}, topics)
}
