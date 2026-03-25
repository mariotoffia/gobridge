// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Target Internal Unit Tests
//
// Tests for unexported helper functions in target.go.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ B001 │ buildMessageAttributes nil metadata    │ PASS     │
// │ B002 │ buildMessageAttributes empty map       │ PASS     │
// │ B003 │ buildMessageAttributes string value    │ PASS     │
// │ B004 │ buildMessageAttributes binary value    │ PASS     │
// │ B005 │ buildMessageAttributes integer value   │ PASS     │
// │ B006 │ buildMessageAttributes float value     │ PASS     │
// │ B007 │ buildMessageAttributes topic added     │ PASS     │
// │ B008 │ buildMessageAttributes skip groupId    │ PASS     │
// │ B009 │ buildMessageAttributes skip retryDelay │ PASS     │
// │ B010 │ buildMessageAttributes mixed types     │ PASS     │
// │ D001 │ generateDeduplicationID deterministic  │ PASS     │
// │ D002 │ generateDeduplicationID diff payload   │ PASS     │
// │ D003 │ generateDeduplicationID diff topic     │ PASS     │
// │ D004 │ generateDeduplicationID diff timestamp │ PASS     │
// │ D005 │ generateDeduplicationID hex format     │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqs

import (
	"regexp"
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// buildMessageAttributes Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestBuildMessageAttributes_NilMetadata validates nil metadata returns nil.
func TestBuildMessageAttributes_NilMetadata(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload:  []byte("test"),
		Metadata: nil,
	}

	result := buildMessageAttributes(msg)

	assert.Nil(t, result, "nil metadata should return nil attributes")
}

// TestBuildMessageAttributes_EmptyMap validates empty map returns nil.
func TestBuildMessageAttributes_EmptyMap(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload:  []byte("test"),
		Metadata: make(map[string]any),
	}

	result := buildMessageAttributes(msg)

	assert.Nil(t, result, "empty metadata should return nil attributes")
}

// TestBuildMessageAttributes_StringValue validates string conversion.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Metadata["key"]    │────▶│  MessageAttributeValue                  │
// │  = "value" (string) │     │  DataType: "String"                     │
// │                     │     │  StringValue: "value"                   │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_StringValue(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"myKey": "myValue",
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)
	require.Contains(t, result, "myKey")
	assert.Equal(t, "String", *result["myKey"].DataType)
	assert.Equal(t, "myValue", *result["myKey"].StringValue)
}

// TestBuildMessageAttributes_BinaryValue validates []byte conversion.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Metadata["key"]    │────▶│  MessageAttributeValue                  │
// │  = []byte{...}      │     │  DataType: "Binary"                     │
// │                     │     │  BinaryValue: []byte{...}               │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_BinaryValue(t *testing.T) {
	binaryData := []byte{0x01, 0x02, 0x03, 0x04}
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"binaryKey": binaryData,
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)
	require.Contains(t, result, "binaryKey")
	assert.Equal(t, "Binary", *result["binaryKey"].DataType)
	assert.Equal(t, binaryData, result["binaryKey"].BinaryValue)
}

// TestBuildMessageAttributes_IntegerValue validates integer conversion.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Metadata["key"]    │────▶│  MessageAttributeValue                  │
// │  = 42 (int)         │     │  DataType: "Number"                     │
// │                     │     │  StringValue: "42"                      │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_IntegerValue(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected string
	}{
		{"int", 42, "42"},
		{"int32", int32(100), "100"},
		{"int64", int64(999), "999"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := bridgeTypes.Message{
				Payload: []byte("test"),
				Metadata: map[string]any{
					"numKey": tc.value,
				},
			}

			result := buildMessageAttributes(msg)

			require.NotNil(t, result)
			require.Contains(t, result, "numKey")
			assert.Equal(t, "Number", *result["numKey"].DataType)
			assert.Equal(t, tc.expected, *result["numKey"].StringValue)
		})
	}
}

// TestBuildMessageAttributes_FloatValue validates float conversion.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Metadata["key"]    │────▶│  MessageAttributeValue                  │
// │  = 3.14 (float64)   │     │  DataType: "Number"                     │
// │                     │     │  StringValue: "3.14"                    │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_FloatValue(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected string
	}{
		{"float32", float32(3.14), "3.14"},
		{"float64", float64(2.718), "2.718"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := bridgeTypes.Message{
				Payload: []byte("test"),
				Metadata: map[string]any{
					"floatKey": tc.value,
				},
			}

			result := buildMessageAttributes(msg)

			require.NotNil(t, result)
			require.Contains(t, result, "floatKey")
			assert.Equal(t, "Number", *result["floatKey"].DataType)
			assert.Contains(t, *result["floatKey"].StringValue, tc.expected[:3]) // Check prefix due to float precision
		})
	}
}

// TestBuildMessageAttributes_TopicAdded validates Topic field is added as attribute.
//
// Note: Topic is only added when metadata is non-empty (function returns early otherwise).
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Message.Topic      │────▶│  MessageAttributeValue["Topic"]         │
// │  = "my-topic"       │     │  DataType: "String"                     │
// │                     │     │  StringValue: "my-topic"                │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_TopicAdded(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Topic:   "my-topic",
		Metadata: map[string]any{
			"someKey": "someValue", // Need at least one entry for Topic to be processed
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)
	require.Contains(t, result, "Topic")
	assert.Equal(t, "String", *result["Topic"].DataType)
	assert.Equal(t, "my-topic", *result["Topic"].StringValue)
}

// TestBuildMessageAttributes_SkipsMessageGroupId validates internal key is skipped.
//
// ═══════════════════════════════════════════════════════════════════════════
// messageGroupId is used internally for FIFO routing → NOT exposed as attribute
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessageAttributes_SkipsMessageGroupId(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"messageGroupId": "group-1",
			"otherKey":       "value",
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)
	assert.NotContains(t, result, "messageGroupId", "messageGroupId should be skipped")
	assert.Contains(t, result, "otherKey", "other keys should be included")
}

// TestBuildMessageAttributes_SkipsRetryDelay validates internal key is skipped.
//
// ═══════════════════════════════════════════════════════════════════════════
// retryDelay is used internally for delayed redelivery → NOT exposed as attribute
// ═══════════════════════════════════════════════════════════════════════════
func TestBuildMessageAttributes_SkipsRetryDelay(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Metadata: map[string]any{
			"retryDelay": int32(60),
			"otherKey":   "value",
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)
	assert.NotContains(t, result, "retryDelay", "retryDelay should be skipped")
	assert.Contains(t, result, "otherKey", "other keys should be included")
}

// TestBuildMessageAttributes_MixedTypes validates multiple types in one message.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Message            │     │  Attributes                             │
// │  ├─ Topic           │────▶│  ├─ "Topic": String                     │
// │  ├─ str: "val"      │────▶│  ├─ "str": String                       │
// │  ├─ num: 42         │────▶│  ├─ "num": Number                       │
// │  ├─ bin: []byte     │────▶│  ├─ "bin": Binary                       │
// │  ├─ messageGroupId  │     │  └─ (skipped)                           │
// │  └─ retryDelay      │     │                                         │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_MixedTypes(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload: []byte("test"),
		Topic:   "test-topic",
		Metadata: map[string]any{
			"strKey":         "stringValue",
			"numKey":         42,
			"binKey":         []byte{0x01, 0x02},
			"messageGroupId": "skip-me",
			"retryDelay":     int32(30),
		},
	}

	result := buildMessageAttributes(msg)

	require.NotNil(t, result)

	// Check Topic
	require.Contains(t, result, "Topic")
	assert.Equal(t, "String", *result["Topic"].DataType)

	// Check string
	require.Contains(t, result, "strKey")
	assert.Equal(t, "String", *result["strKey"].DataType)

	// Check number
	require.Contains(t, result, "numKey")
	assert.Equal(t, "Number", *result["numKey"].DataType)

	// Check binary
	require.Contains(t, result, "binKey")
	assert.Equal(t, "Binary", *result["binKey"].DataType)

	// Verify skipped keys
	assert.NotContains(t, result, "messageGroupId")
	assert.NotContains(t, result, "retryDelay")

	// Verify count (Topic + 3 keys = 4)
	assert.Len(t, result, 4)
}

// ═══════════════════════════════════════════════════════════════════════════
// generateDeduplicationID Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGenerateDeduplicationID_Deterministic validates same input produces same output.
func TestGenerateDeduplicationID_Deterministic(t *testing.T) {
	fixedTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	msg := bridgeTypes.Message{
		Payload:   []byte("test-payload"),
		Topic:     "test-topic",
		CreatedAt: fixedTime,
	}

	id1 := generateDeduplicationID(msg)
	id2 := generateDeduplicationID(msg)

	assert.Equal(t, id1, id2, "same input should produce same deduplication ID")
}

// TestGenerateDeduplicationID_DifferentPayload validates different payload produces different ID.
func TestGenerateDeduplicationID_DifferentPayload(t *testing.T) {
	fixedTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	msg1 := bridgeTypes.Message{
		Payload:   []byte("payload-1"),
		Topic:     "test-topic",
		CreatedAt: fixedTime,
	}
	msg2 := bridgeTypes.Message{
		Payload:   []byte("payload-2"),
		Topic:     "test-topic",
		CreatedAt: fixedTime,
	}

	id1 := generateDeduplicationID(msg1)
	id2 := generateDeduplicationID(msg2)

	assert.NotEqual(t, id1, id2, "different payload should produce different ID")
}

// TestGenerateDeduplicationID_DifferentTopic validates different topic produces different ID.
func TestGenerateDeduplicationID_DifferentTopic(t *testing.T) {
	fixedTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	msg1 := bridgeTypes.Message{
		Payload:   []byte("test-payload"),
		Topic:     "topic-1",
		CreatedAt: fixedTime,
	}
	msg2 := bridgeTypes.Message{
		Payload:   []byte("test-payload"),
		Topic:     "topic-2",
		CreatedAt: fixedTime,
	}

	id1 := generateDeduplicationID(msg1)
	id2 := generateDeduplicationID(msg2)

	assert.NotEqual(t, id1, id2, "different topic should produce different ID")
}

// TestGenerateDeduplicationID_DifferentTimestamp validates different time produces different ID.
//
// This ensures identical messages sent at different times get unique dedup IDs.
func TestGenerateDeduplicationID_DifferentTimestamp(t *testing.T) {
	msg1 := bridgeTypes.Message{
		Payload:   []byte("test-payload"),
		Topic:     "test-topic",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}
	msg2 := bridgeTypes.Message{
		Payload:   []byte("test-payload"),
		Topic:     "test-topic",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 1, 0, time.UTC), // 1 second later
	}

	id1 := generateDeduplicationID(msg1)
	id2 := generateDeduplicationID(msg2)

	assert.NotEqual(t, id1, id2, "different timestamp should produce different ID")
}

// TestGenerateDeduplicationID_ValidHexFormat validates output is 32-char hex string.
//
// MD5 produces 128-bit hash → 32 hex characters
func TestGenerateDeduplicationID_ValidHexFormat(t *testing.T) {
	msg := bridgeTypes.Message{
		Payload:   []byte("test"),
		Topic:     "topic",
		CreatedAt: time.Now(),
	}

	id := generateDeduplicationID(msg)

	// MD5 hash is 32 hex characters
	assert.Len(t, id, 32, "deduplication ID should be 32 characters")

	// Verify it's valid hex
	hexPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	assert.True(t, hexPattern.MatchString(id), "deduplication ID should be valid lowercase hex")
}
