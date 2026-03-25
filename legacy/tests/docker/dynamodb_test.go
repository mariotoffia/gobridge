// ═══════════════════════════════════════════════════════════════════════════
// Docker Test Utilities - DynamoDB Unit Tests
//
// Tests for DynamoDBBuilder and DynamoDBContainer.
// These tests do NOT require Docker to be running.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ D001 │ DynamoDBBuilder has sensible defaults  │ PASS     │
// │ D002 │ DynamoDBBuilder chaining works         │ PASS     │
// │ D003 │ DataDir disables InMemory              │ PASS     │
// │ D004 │ Endpoint generation                    │ PASS     │
// │ D005 │ Helper constructors                    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// DynamoDBBuilder Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDynamoDBBuilder_Defaults validates sensible defaults.
func TestDynamoDBBuilder_Defaults(t *testing.T) {
	builder := NewDynamoDB()

	assert.NotEmpty(t, builder.image, "image should have a default")
	assert.Equal(t, 0, builder.port, "port should default to 0 (random)")
	assert.True(t, builder.sharedDb, "sharedDb should default to true")
	assert.True(t, builder.inMemory, "inMemory should default to true")
	assert.Greater(t, builder.readyTimeout.Seconds(), float64(0), "readyTimeout should be positive")
}

// TestDynamoDBBuilder_Chaining validates fluent method chaining.
func TestDynamoDBBuilder_Chaining(t *testing.T) {
	builder := NewDynamoDB().
		Image("amazon/dynamodb-local:2.0.0").
		Name("test-dynamodb").
		Port(8000).
		SharedDb(false).
		InMemory(false)

	assert.Equal(t, "amazon/dynamodb-local:2.0.0", builder.image)
	assert.Equal(t, "test-dynamodb", builder.name)
	assert.Equal(t, 8000, builder.port)
	assert.False(t, builder.sharedDb)
	assert.False(t, builder.inMemory)
}

// TestDynamoDBBuilder_DataDir_DisablesInMemory validates DataDir behavior.
func TestDynamoDBBuilder_DataDir_DisablesInMemory(t *testing.T) {
	builder := NewDynamoDB().
		InMemory(true).
		DataDir("/data/dynamodb")

	assert.False(t, builder.inMemory, "DataDir should disable inMemory")
	assert.Equal(t, "/data/dynamodb", builder.dataDir)
}

// ═══════════════════════════════════════════════════════════════════════════
// DynamoDBContainer Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDynamoDBContainer_Endpoint validates endpoint generation.
func TestDynamoDBContainer_Endpoint(t *testing.T) {
	container := &DynamoDBContainer{
		port: 38000,
	}
	assert.Equal(t, "http://localhost:38000", container.Endpoint())
}

// TestDynamoDBContainer_Port validates port accessor.
func TestDynamoDBContainer_Port(t *testing.T) {
	container := &DynamoDBContainer{
		port: 38000,
	}
	assert.Equal(t, 38000, container.Port())
}

// ═══════════════════════════════════════════════════════════════════════════
// Helper Constructor Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDynamoDBHelpers validates convenience constructors.
func TestDynamoDBHelpers(t *testing.T) {
	t.Run("DefaultDynamoDBConfig", func(t *testing.T) {
		builder := DefaultDynamoDBConfig()
		assert.True(t, builder.sharedDb)
		assert.True(t, builder.inMemory)
	})

	t.Run("DynamoDBWithPersistence", func(t *testing.T) {
		builder := DynamoDBWithPersistence("/tmp/data")
		assert.True(t, builder.sharedDb)
		assert.False(t, builder.inMemory)
		assert.Equal(t, "/tmp/data", builder.dataDir)
	})
}

