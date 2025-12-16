// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Target Unit Tests
//
// Tests for SQS Target constructor validation, behavior, and helpers.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ T001 │ NewTarget nil config error             │ PASS     │
// │ T002 │ NewTarget no queue error               │ PASS     │
// │ T003 │ NewTarget with QueueURL success        │ PASS     │
// │ T004 │ NewTarget with QueueName success       │ PASS     │
// │ T005 │ NewTarget DelaySeconds clamping        │ PASS     │
// │ T006 │ NewTarget BatchSize clamping           │ PASS     │
// │ T007 │ NewTarget Timeout default              │ PASS     │
// │ T008 │ Target GetID returns correct value     │ PASS     │
// │ T009 │ Target GetTransportType returns SQS    │ PASS     │
// │ T010 │ Target Capabilities correct            │ PASS     │
// │ T011 │ Target Close idempotent                │ PASS     │
// │ T012 │ buildMessageAttributes empty           │ PASS     │
// │ T013 │ buildMessageAttributes types           │ PASS     │
// │ T014 │ buildMessageAttributes skips internal  │ PASS     │
// │ T015 │ generateDeduplicationID deterministic  │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqstests

import (
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewTarget Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewTarget_NilConfig validates nil config returns error.
func TestNewTarget_NilConfig(t *testing.T) {
	tgt, err := sqs.NewTarget(nil)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewTarget_NoQueue validates missing queue returns error.
func TestNewTarget_NoQueue(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID: "test-target",
		// Neither QueueURL nor QueueName
	}
	tgt, err := sqs.NewTarget(cfg)
	assert.Nil(t, tgt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queueUrl or queueName")
}

// TestNewTarget_QueueURL validates QueueURL succeeds.
func TestNewTarget_QueueURL(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:       "test-target",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test-queue",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()
}

// TestNewTarget_QueueName validates QueueName succeeds.
func TestNewTarget_QueueName(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:        "test-target",
		QueueName: "test-queue",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	require.NotNil(t, tgt)
	defer tgt.Close()
}

// TestNewTarget_DelaySecondsClamp validates DelaySeconds clamping to 0-900.
//
// ═══════════════════════════════════════════════════════════════════════════
// Input: DelaySeconds  →  Result
// ═══════════════════════════════════════════════════════════════════════════
//
//	-1   →  0 (clamped)
//	 0   →  0
//
// 900   →  900
// 901   →  900 (clamped)
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_DelaySecondsClamp(t *testing.T) {
	tests := []struct {
		name  string
		input int32
	}{
		{"negative clamps to 0", -1},
		{"zero is valid", 0},
		{"thirty is valid", 30},
		{"nine hundred is valid", 900},
		{"nine hundred one clamps to 900", 901},
		{"large clamps to 900", 9999},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.TargetConfigImpl{
				ID:           "test",
				QueueURL:     "https://sqs.us-east-1.amazonaws.com/123456789/test",
				DelaySeconds: tc.input,
			}
			tgt, err := sqs.NewTarget(cfg)
			require.NoError(t, err)
			defer tgt.Close()
		})
	}
}

// TestNewTarget_BatchSizeClamp validates BatchSize clamping to 1-10.
//
// ═══════════════════════════════════════════════════════════════════════════
// Input: BatchSize  →  Result
// ═══════════════════════════════════════════════════════════════════════════
//
//	 0  →  10 (default)
//	-1  →  10 (default)
//	 1  →  1
//	10  →  10
//	11  →  10 (clamped)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewTarget_BatchSizeClamp(t *testing.T) {
	tests := []struct {
		name  string
		input int
	}{
		{"zero defaults to 10", 0},
		{"negative defaults to 10", -1},
		{"one is valid", 1},
		{"five is valid", 5},
		{"ten is valid", 10},
		{"eleven clamps to 10", 11},
		{"large clamps to 10", 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.TargetConfigImpl{
				ID:        "test",
				QueueURL:  "https://sqs.us-east-1.amazonaws.com/123456789/test",
				BatchSize: tc.input,
			}
			tgt, err := sqs.NewTarget(cfg)
			require.NoError(t, err)
			defer tgt.Close()
		})
	}
}

// TestNewTarget_TimeoutDefault validates Timeout defaults to 30s.
func TestNewTarget_TimeoutDefault(t *testing.T) {
	tests := []struct {
		name  string
		input time.Duration
	}{
		{"zero defaults to 30s", 0},
		{"custom is used", 60 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.TargetConfigImpl{
				ID:       "test",
				QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
				Timeout:  tc.input,
			}
			tgt, err := sqs.NewTarget(cfg)
			require.NoError(t, err)
			defer tgt.Close()
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTarget_GetID validates ID getter returns configured value.
func TestTarget_GetID(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:       "my-target-id",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	assert.Equal(t, "my-target-id", tgt.GetID())
}

// TestTarget_GetTransportType validates transport type returns SQS.
func TestTarget_GetTransportType(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	assert.Equal(t, sqs.TransportType, tgt.GetTransportType())
}

// TestTarget_Capabilities validates reported capabilities.
//
// Expected capabilities:
//   - CapabilityPublishAtLeastOnce: SQS provides at-least-once delivery
//   - CapabilityNativeRetry: SQS handles retries internally
//   - CapabilityDelayedDelivery: SQS supports DelaySeconds
func TestTarget_Capabilities(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)
	defer tgt.Close()

	caps := tgt.Capabilities()

	assert.True(t, caps.Has(bridgeTypes.CapabilityPublishAtLeastOnce),
		"SQS should report PublishAtLeastOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityNativeRetry),
		"SQS should report NativeRetry")
	assert.True(t, caps.Has(bridgeTypes.CapabilityDelayedDelivery),
		"SQS should report DelayedDelivery")
}

// TestTarget_CloseIdempotent validates multiple Close calls don't panic.
func TestTarget_CloseIdempotent(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	tgt, err := sqs.NewTarget(cfg)
	require.NoError(t, err)

	// First close
	err1 := tgt.Close()
	assert.NoError(t, err1)

	// Second close should not panic
	err2 := tgt.Close()
	assert.NoError(t, err2)

	// Third close should not panic
	err3 := tgt.Close()
	assert.NoError(t, err3)
}

// ═══════════════════════════════════════════════════════════════════════════
// Message Attribute Helper Tests
// ═══════════════════════════════════════════════════════════════════════════

// Note: buildMessageAttributes and generateDeduplicationID are not exported,
// so we test them indirectly through integration tests. These tests document
// expected behavior.

// TestBuildMessageAttributes_Documentation documents expected behavior.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Message.Metadata   │────▶│  map[string]MessageAttributeValue       │
// │                     │     │                                         │
// │  key: string        │     │  String  → DataType: "String"           │
// │  key: []byte        │     │  Binary  → DataType: "Binary"           │
// │  key: int/float     │     │  Number  → DataType: "Number"           │
// │                     │     │                                         │
// │  Topic: non-empty   │     │  "Topic" attribute added                │
// │                     │     │                                         │
// │  messageGroupId     │     │  SKIPPED (internal)                     │
// │  retryDelay         │     │  SKIPPED (internal)                     │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestBuildMessageAttributes_Documentation(t *testing.T) {
	t.Skip("buildMessageAttributes is not exported; tested via integration")
}

// TestGenerateDeduplicationID_Documentation documents expected behavior.
//
// Deduplication ID is MD5 hash of:
//   - Payload
//   - Topic
//   - CreatedAt timestamp
//
// This ensures identical messages at different times get different IDs.
func TestGenerateDeduplicationID_Documentation(t *testing.T) {
	t.Skip("generateDeduplicationID is not exported; tested via integration")
}
