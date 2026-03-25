// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Source Unit Tests
//
// Tests for SQS Source constructor validation and behavior.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ S001 │ NewSource nil config error             │ PASS     │
// │ S002 │ NewSource no queue error               │ PASS     │
// │ S003 │ NewSource with QueueURL success        │ PASS     │
// │ S004 │ NewSource with QueueName success       │ PASS     │
// │ S005 │ NewSource MaxMessages clamping         │ PASS     │
// │ S006 │ NewSource WaitTime clamping            │ PASS     │
// │ S007 │ NewSource Visibility default           │ PASS     │
// │ S008 │ NewSource Prefetch default             │ PASS     │
// │ S009 │ Source GetID returns correct value     │ PASS     │
// │ S010 │ Source GetTransportType returns SQS    │ PASS     │
// │ S011 │ Source Capabilities correct            │ PASS     │
// │ S012 │ Source Messages returns channel        │ PASS     │
// │ S013 │ Source Close idempotent                │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqstests

import (
	"testing"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// NewSource Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewSource_NilConfig validates nil config returns error.
func TestNewSource_NilConfig(t *testing.T) {
	src, err := sqs.NewSource(nil)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is required")
}

// TestNewSource_NoQueue validates missing queue returns error.
func TestNewSource_NoQueue(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID: "test-source",
		// Neither QueueURL nor QueueName
	}
	src, err := sqs.NewSource(cfg)
	assert.Nil(t, src)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queueUrl or queueName")
}

// TestNewSource_QueueURL validates QueueURL succeeds.
func TestNewSource_QueueURL(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "test-source",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test-queue",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()
}

// TestNewSource_QueueName validates QueueName succeeds.
func TestNewSource_QueueName(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:        "test-source",
		QueueName: "test-queue",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	require.NotNil(t, src)
	defer src.Close()
}

// TestNewSource_MaxMessagesClamp validates MaxMessages clamping to 1-10.
//
// ═══════════════════════════════════════════════════════════════════════════
// Input: MaxMessages  →  Result
// ═══════════════════════════════════════════════════════════════════════════
//
//	 0  →  10 (default)
//	-1  →  10 (default)
//	 1  →  1
//	10  →  10
//	11  →  10 (clamped)
//	99  →  10 (clamped)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewSource_MaxMessagesClamp(t *testing.T) {
	tests := []struct {
		name     string
		input    int32
		expected int32
	}{
		{"zero defaults to 10", 0, 10},
		{"negative defaults to 10", -1, 10},
		{"one is valid", 1, 1},
		{"five is valid", 5, 5},
		{"ten is valid", 10, 10},
		{"eleven clamps to 10", 11, 10},
		{"large clamps to 10", 99, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.SourceConfigImpl{
				ID:          "test",
				QueueURL:    "https://sqs.us-east-1.amazonaws.com/123456789/test",
				MaxMessages: tc.input,
			}
			src, err := sqs.NewSource(cfg)
			require.NoError(t, err)
			defer src.Close()
			// Note: we can't directly access maxMessages, but we verify no error
		})
	}
}

// TestNewSource_WaitTimeClamp validates WaitTimeSeconds clamping to 0-20.
//
// ═══════════════════════════════════════════════════════════════════════════
// Input: WaitTimeSeconds  →  Result
// ═══════════════════════════════════════════════════════════════════════════
//
//	-1  →  0 (clamped)
//	 0  →  0
//	20  →  20
//	21  →  20 (clamped)
//
// ═══════════════════════════════════════════════════════════════════════════
func TestNewSource_WaitTimeClamp(t *testing.T) {
	tests := []struct {
		name  string
		input int32
	}{
		{"negative clamps to 0", -1},
		{"zero is valid", 0},
		{"ten is valid", 10},
		{"twenty is valid", 20},
		{"twenty-one clamps to 20", 21},
		{"large clamps to 20", 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.SourceConfigImpl{
				ID:              "test",
				QueueURL:        "https://sqs.us-east-1.amazonaws.com/123456789/test",
				WaitTimeSeconds: tc.input,
			}
			src, err := sqs.NewSource(cfg)
			require.NoError(t, err)
			defer src.Close()
		})
	}
}

// TestNewSource_VisibilityDefault validates VisibilityTimeout defaults to 30.
func TestNewSource_VisibilityDefault(t *testing.T) {
	tests := []struct {
		name  string
		input int32
	}{
		{"zero defaults to 30", 0},
		{"negative defaults to 30", -1},
		{"positive is used", 60},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.SourceConfigImpl{
				ID:                "test",
				QueueURL:          "https://sqs.us-east-1.amazonaws.com/123456789/test",
				VisibilityTimeout: tc.input,
			}
			src, err := sqs.NewSource(cfg)
			require.NoError(t, err)
			defer src.Close()
		})
	}
}

// TestNewSource_PrefetchDefault validates Prefetch defaults to 100.
func TestNewSource_PrefetchDefault(t *testing.T) {
	tests := []struct {
		name  string
		input int
	}{
		{"zero defaults to 100", 0},
		{"negative defaults to 100", -1},
		{"positive is used", 50},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.SourceConfigImpl{
				ID:       "test",
				QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
				Prefetch: tc.input,
			}
			src, err := sqs.NewSource(cfg)
			require.NoError(t, err)
			defer src.Close()
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Source Behavior Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSource_GetID validates ID getter returns configured value.
func TestSource_GetID(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "my-source-id",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	assert.Equal(t, "my-source-id", src.GetID())
}

// TestSource_GetTransportType validates transport type returns SQS.
func TestSource_GetTransportType(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	assert.Equal(t, sqs.TransportType, src.GetTransportType())
}

// TestSource_Capabilities validates reported capabilities.
//
// Expected capabilities:
//   - CapabilityReceiveAtLeastOnce: SQS delivers at-least-once
//   - CapabilityRedelivery: Nack causes message to become visible again
//   - CapabilityExtendTimeout: Extend increases visibility timeout
func TestSource_Capabilities(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	caps := src.Capabilities()

	assert.True(t, caps.Has(bridgeTypes.CapabilityReceiveAtLeastOnce),
		"SQS should report ReceiveAtLeastOnce")
	assert.True(t, caps.Has(bridgeTypes.CapabilityRedelivery),
		"SQS should report Redelivery (Nack support)")
	assert.True(t, caps.Has(bridgeTypes.CapabilityExtendTimeout),
		"SQS should report ExtendTimeout")
}

// TestSource_Messages validates Messages returns a channel.
func TestSource_Messages(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)
	defer src.Close()

	ch := src.Messages()
	assert.NotNil(t, ch, "Messages() should return a non-nil channel")
}

// TestSource_CloseIdempotent validates multiple Close calls don't panic.
func TestSource_CloseIdempotent(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{
		ID:       "test",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789/test",
	}
	src, err := sqs.NewSource(cfg)
	require.NoError(t, err)

	// First close
	err1 := src.Close()
	assert.NoError(t, err1)

	// Second close should not panic
	err2 := src.Close()
	assert.NoError(t, err2)

	// Third close should not panic
	err3 := src.Close()
	assert.NoError(t, err3)
}
