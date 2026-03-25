// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Configuration Unit Tests
//
// Tests for SourceConfigImpl and TargetConfigImpl configuration types.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ C001 │ SourceConfigImpl interface compliance  │ PASS     │
// │ C002 │ SourceConfigImpl getters               │ PASS     │
// │ C003 │ TargetConfigImpl interface compliance  │ PASS     │
// │ C004 │ TargetConfigImpl getters               │ PASS     │
// │ C005 │ TransportType constant                 │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqstests

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/aws/sqs"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// TransportType Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTransportType_Constant validates the transport type constant.
func TestTransportType_Constant(t *testing.T) {
	assert.Equal(t, types.TransportType("SQS"), sqs.TransportType)
}

// ═══════════════════════════════════════════════════════════════════════════
// SourceConfigImpl Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceConfigImpl_Interface verifies SourceConfigImpl implements types.SourceConfig.
func TestSourceConfigImpl_Interface(t *testing.T) {
	var _ types.SourceConfig = (*sqs.SourceConfigImpl)(nil)
}

// TestSourceConfigImpl_GetID validates the ID getter.
func TestSourceConfigImpl_GetID(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{ID: "test-source-123"}
	assert.Equal(t, "test-source-123", cfg.GetID())
}

// TestSourceConfigImpl_GetTransportType validates the transport type.
func TestSourceConfigImpl_GetTransportType(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{}
	assert.Equal(t, sqs.TransportType, cfg.GetTransportType())
}

// TestSourceConfigImpl_GetQoS validates QoS returns at-least-once.
func TestSourceConfigImpl_GetQoS(t *testing.T) {
	cfg := &sqs.SourceConfigImpl{}
	qos := cfg.GetQoS()
	assert.NotNil(t, qos)
	assert.Equal(t, 1, qos.Level, "SQS is at-least-once")
}

// TestSourceConfigImpl_GetPrefetch validates prefetch getter.
func TestSourceConfigImpl_GetPrefetch(t *testing.T) {
	tests := []struct {
		name     string
		prefetch int
		expected int
	}{
		{"zero", 0, 0},
		{"positive", 50, 50},
		{"large", 1000, 1000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &sqs.SourceConfigImpl{Prefetch: tc.prefetch}
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
	cfg := &sqs.SourceConfigImpl{Resources: resources}
	assert.Equal(t, resources, cfg.GetResources())
}

// TestSourceConfigImpl_AllowMultipleResourceMatches validates allow multiple flag.
func TestSourceConfigImpl_AllowMultipleResourceMatches(t *testing.T) {
	t.Run("false", func(t *testing.T) {
		cfg := &sqs.SourceConfigImpl{AllowMultiple: false}
		assert.False(t, cfg.AllowMultipleResourceMatches())
	})

	t.Run("true", func(t *testing.T) {
		cfg := &sqs.SourceConfigImpl{AllowMultiple: true}
		assert.True(t, cfg.AllowMultipleResourceMatches())
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetConfigImpl Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetConfigImpl_Interface verifies TargetConfigImpl implements types.TargetConfig.
func TestTargetConfigImpl_Interface(t *testing.T) {
	var _ types.TargetConfig = (*sqs.TargetConfigImpl)(nil)
}

// TestTargetConfigImpl_GetID validates the ID getter.
func TestTargetConfigImpl_GetID(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{ID: "test-target-456"}
	assert.Equal(t, "test-target-456", cfg.GetID())
}

// TestTargetConfigImpl_GetTransportType validates the transport type.
func TestTargetConfigImpl_GetTransportType(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{}
	assert.Equal(t, sqs.TransportType, cfg.GetTransportType())
}

// TestTargetConfigImpl_GetDefaultQoS validates QoS returns at-least-once.
func TestTargetConfigImpl_GetDefaultQoS(t *testing.T) {
	cfg := &sqs.TargetConfigImpl{}
	qos := cfg.GetDefaultQoS()
	assert.NotNil(t, qos)
	assert.Equal(t, 1, qos.Level, "SQS is at-least-once")
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
			cfg := &sqs.TargetConfigImpl{BatchSize: tc.batchSize}
			assert.Equal(t, tc.expected, cfg.GetBatchSize())
		})
	}
}

// TestTargetConfigImpl_GetTimeout validates timeout getter.
func TestTargetConfigImpl_GetTimeout(t *testing.T) {
	t.Run("zero returns nil", func(t *testing.T) {
		cfg := &sqs.TargetConfigImpl{Timeout: 0}
		assert.Nil(t, cfg.GetTimeout())
	})

	t.Run("non-zero returns pointer", func(t *testing.T) {
		timeout := 30 * time.Second
		cfg := &sqs.TargetConfigImpl{Timeout: timeout}
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
	cfg := &sqs.TargetConfigImpl{Resources: resources}
	assert.Equal(t, resources, cfg.GetResources())
}

// TestTargetConfigImpl_AllowMultipleResourceMatches validates allow multiple flag.
func TestTargetConfigImpl_AllowMultipleResourceMatches(t *testing.T) {
	t.Run("false", func(t *testing.T) {
		cfg := &sqs.TargetConfigImpl{AllowMultiple: false}
		assert.False(t, cfg.AllowMultipleResourceMatches())
	})

	t.Run("true", func(t *testing.T) {
		cfg := &sqs.TargetConfigImpl{AllowMultiple: true}
		assert.True(t, cfg.AllowMultipleResourceMatches())
	})
}
