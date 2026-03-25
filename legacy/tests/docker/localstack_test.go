// ═══════════════════════════════════════════════════════════════════════════
// Docker Test Utilities - LocalStack Unit Tests
//
// Tests for LocalStackBuilder and LocalStackContainer.
// These tests do NOT require Docker to be running.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ L001 │ LocalStackBuilder accumulates services │ PASS     │
// │ L002 │ LocalStackBuilder has sensible default │ PASS     │
// │ L003 │ LocalStackBuilder chaining works       │ PASS     │
// │ L004 │ HasService case insensitive            │ PASS     │
// │ L005 │ Endpoint generation                    │ PASS     │
// │ L006 │ Helper constructors                    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// LocalStackBuilder Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestLocalStackBuilder_ServiceAccumulation validates service builder methods.
func TestLocalStackBuilder_ServiceAccumulation(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*LocalStackBuilder) *LocalStackBuilder
		expected []string
	}{
		{
			name: "single service",
			setup: func(b *LocalStackBuilder) *LocalStackBuilder {
				return b.WithSQS()
			},
			expected: []string{"sqs"},
		},
		{
			name: "multiple services chained",
			setup: func(b *LocalStackBuilder) *LocalStackBuilder {
				return b.WithSQS().WithDynamoDB().WithS3()
			},
			expected: []string{"sqs", "dynamodb", "s3"},
		},
		{
			name: "all common services",
			setup: func(b *LocalStackBuilder) *LocalStackBuilder {
				return b.WithSQS().WithDynamoDB().WithS3().WithSNS().WithKinesis().WithLambda()
			},
			expected: []string{"sqs", "dynamodb", "s3", "sns", "kinesis", "lambda"},
		},
		{
			name: "WithServices helper",
			setup: func(b *LocalStackBuilder) *LocalStackBuilder {
				return b.WithServices("sqs", "dynamodb", "secretsmanager")
			},
			expected: []string{"sqs", "dynamodb", "secretsmanager"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := NewLocalStack()
			builder = tc.setup(builder)
			assert.Equal(t, tc.expected, builder.services)
		})
	}
}

// TestLocalStackBuilder_Defaults validates sensible defaults.
func TestLocalStackBuilder_Defaults(t *testing.T) {
	builder := NewLocalStack()

	assert.NotEmpty(t, builder.image, "image should have a default")
	assert.Equal(t, 0, builder.edgePort, "edgePort should default to 0 (random)")
	assert.Empty(t, builder.services, "services should be empty by default")
	assert.False(t, builder.debug, "debug should default to false")
	assert.Greater(t, builder.readyTimeout.Seconds(), float64(0), "readyTimeout should be positive")
}

// TestLocalStackBuilder_Chaining validates fluent method chaining.
func TestLocalStackBuilder_Chaining(t *testing.T) {
	builder := NewLocalStack().
		Image("localstack/localstack:3.0").
		Name("test-localstack").
		EdgePort(4566).
		WithSQS().
		WithDynamoDB().
		Debug(true).
		DataDir("/tmp/localstack").
		Env("PERSISTENCE", "1")

	assert.Equal(t, "localstack/localstack:3.0", builder.image)
	assert.Equal(t, "test-localstack", builder.name)
	assert.Equal(t, 4566, builder.edgePort)
	assert.Contains(t, builder.services, "sqs")
	assert.Contains(t, builder.services, "dynamodb")
	assert.True(t, builder.debug)
	assert.Equal(t, "/tmp/localstack", builder.dataDir)
	assert.Equal(t, "1", builder.env["PERSISTENCE"])
}

// ═══════════════════════════════════════════════════════════════════════════
// LocalStackContainer Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestLocalStackContainer_HasService validates service lookup.
func TestLocalStackContainer_HasService(t *testing.T) {
	container := &LocalStackContainer{
		services: []string{"sqs", "dynamodb", "s3"},
	}

	tests := []struct {
		service  string
		expected bool
	}{
		{"sqs", true},
		{"SQS", true}, // Case insensitive
		{"dynamodb", true},
		{"DynamoDB", true},
		{"s3", true},
		{"sns", false},
		{"lambda", false},
	}

	for _, tc := range tests {
		t.Run(tc.service, func(t *testing.T) {
			assert.Equal(t, tc.expected, container.HasService(tc.service))
		})
	}
}

// TestLocalStackContainer_Endpoints validates endpoint generation.
func TestLocalStackContainer_Endpoints(t *testing.T) {
	container := &LocalStackContainer{
		edgePort: 34566,
		services: []string{"sqs", "dynamodb", "s3", "sns"},
	}

	assert.Equal(t, 34566, container.EdgePort())
	assert.Equal(t, "http://localhost:34566", container.Endpoint())
	assert.Equal(t, "http://localhost:34566", container.SQSEndpoint())
	assert.Equal(t, "http://localhost:34566", container.DynamoDBEndpoint())
	assert.Equal(t, "http://localhost:34566", container.S3Endpoint())
	assert.Equal(t, "http://localhost:34566", container.SNSEndpoint())
}

// TestLocalStackContainer_Services validates services accessor.
func TestLocalStackContainer_Services(t *testing.T) {
	container := &LocalStackContainer{
		services: []string{"sqs", "dynamodb"},
	}

	services := container.Services()
	assert.Len(t, services, 2)
	assert.Contains(t, services, "sqs")
	assert.Contains(t, services, "dynamodb")
}

// ═══════════════════════════════════════════════════════════════════════════
// Helper Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestLocalStackHelpers validates convenience constructors.
func TestLocalStackHelpers(t *testing.T) {
	t.Run("DefaultLocalStackConfig", func(t *testing.T) {
		builder := DefaultLocalStackConfig()
		assert.Contains(t, builder.services, "sqs")
		assert.Contains(t, builder.services, "dynamodb")
	})

	t.Run("LocalStackForSQS", func(t *testing.T) {
		builder := LocalStackForSQS()
		assert.Contains(t, builder.services, "sqs")
		assert.Len(t, builder.services, 1)
	})

	t.Run("LocalStackForDynamoDB", func(t *testing.T) {
		builder := LocalStackForDynamoDB()
		assert.Contains(t, builder.services, "dynamodb")
		assert.Len(t, builder.services, 1)
	})

	t.Run("LocalStackFull", func(t *testing.T) {
		builder := LocalStackFull()
		assert.Contains(t, builder.services, "sqs")
		assert.Contains(t, builder.services, "dynamodb")
		assert.Contains(t, builder.services, "s3")
		assert.Contains(t, builder.services, "sns")
	})
}

