// ═══════════════════════════════════════════════════════════════════════════
// Docker Test Utilities - Integration Tests
//
// These tests require Docker to be running. They verify actual container
// lifecycle and connectivity.
//
// Run with: go test -v -tags=integration ./tests/docker/...
// Skip with: go test -short ./tests/docker/...
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ I001 │ Container start/stop lifecycle         │ PASS     │
// │ I002 │ Dynamic port allocation                │ PASS     │
// │ I003 │ Container labels                       │ PASS     │
// │ I004 │ Mosquitto container start              │ PASS     │
// │ I005 │ LocalStack container start             │ PASS     │
// │ I006 │ DynamoDB container start               │ PASS     │
// │ I007 │ Orphan cleanup                         │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfNoDocker skips the test if Docker is not available.
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	RequireDocker(t)
}

// ═══════════════════════════════════════════════════════════════════════════
// Container Lifecycle Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_Container_StartStop validates basic container lifecycle.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Start() → IsRunning() = true → Stop() → IsRunning() = false → Remove()
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_Container_StartStop(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start a simple container
	container, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start container")
	defer container.Remove(ctx)

	// Verify it's running
	assert.True(t, container.IsRunning(ctx), "Container should be running")
	assert.NotEmpty(t, container.ID, "Container ID should be set")

	// Stop the container
	err = container.Stop(ctx)
	require.NoError(t, err, "Failed to stop container")

	// Give it a moment to stop
	time.Sleep(500 * time.Millisecond)

	// Verify it's stopped
	assert.False(t, container.IsRunning(ctx), "Container should be stopped")
}

// TestIntegration_Container_DynamicPort validates random port allocation.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Port(0, 80) → Start() → GetHostPort(80) → port > 0
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_Container_DynamicPort(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start nginx with random port
	container, err := NewContainerBuilder().
		Image("nginx:alpine").
		Port(0, 80). // Random port
		ReadyTimeout(30 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start container")
	defer container.Remove(ctx)

	// Verify port was resolved
	hostPort := container.GetHostPort(80)
	assert.Greater(t, hostPort, 0, "Host port should be resolved")
	assert.Less(t, hostPort, 65536, "Host port should be valid")

	// Verify we can connect
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", hostPort), 2*time.Second)
	require.NoError(t, err, "Should be able to connect to container")
	conn.Close()
}

// TestIntegration_Container_Labels validates label application and filtering.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	Container with custom labels → Verify standard labels applied
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_Container_Labels(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start container with custom label
	container, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		Label("custom.label", "test-value").
		ServiceType("test-service").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start container")
	defer container.Remove(ctx)

	// Verify labels via docker inspect
	cli := NewDockerCLI()

	// Check standard test label
	output, err := cli.Run(ctx, "inspect", "-f", fmt.Sprintf("{{index .Config.Labels \"%s\"}}", LabelTest), container.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", output, "Test label should be set")

	// Check session label
	output, err = cli.Run(ctx, "inspect", "-f", fmt.Sprintf("{{index .Config.Labels \"%s\"}}", LabelSession), container.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, output, "Session label should be set")

	// Check service label
	output, err = cli.Run(ctx, "inspect", "-f", fmt.Sprintf("{{index .Config.Labels \"%s\"}}", LabelService), container.ID)
	require.NoError(t, err)
	assert.Equal(t, "test-service", output, "Service label should be set")

	// Check custom label
	output, err = cli.Run(ctx, "inspect", "-f", "{{index .Config.Labels \"custom.label\"}}", container.ID)
	require.NoError(t, err)
	assert.Equal(t, "test-value", output, "Custom label should be set")
}

// TestIntegration_Container_Exec validates executing commands inside container.
func TestIntegration_Container_Exec(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	container, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err)
	defer container.Remove(ctx)

	// Execute a command
	output, err := container.Exec(ctx, "echo", "hello world")
	require.NoError(t, err)
	assert.Contains(t, output, "hello world")
}

// TestIntegration_Container_Logs validates log retrieval.
func TestIntegration_Container_Logs(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	container, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sh", "-c", "echo 'test log message' && sleep 300").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err)
	defer container.Remove(ctx)

	// Wait for log to be written
	time.Sleep(500 * time.Millisecond)

	logs, err := container.Logs(ctx)
	require.NoError(t, err)
	assert.Contains(t, logs, "test log message")
}

// ═══════════════════════════════════════════════════════════════════════════
// Mosquitto Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_Mosquitto_Start validates Mosquitto container startup.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	NewMosquitto() → Start() → BrokerURL() → TCP connect → Remove()
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_Mosquitto_Start(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start Mosquitto
	mosquitto, err := NewMosquitto().
		AllowAnonymous(true).
		ReadyTimeout(30 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start Mosquitto")
	defer mosquitto.Remove(ctx)

	// Verify port is set
	assert.Greater(t, mosquitto.MQTTPort(), 0, "MQTT port should be set")

	// Verify we can connect
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", mosquitto.MQTTPort()), 2*time.Second)
	require.NoError(t, err, "Should be able to connect to MQTT broker")
	conn.Close()

	// Verify URL format
	assert.Contains(t, mosquitto.BrokerURL(), "tcp://127.0.0.1:")
}

// TestIntegration_Mosquitto_WebSocket validates WebSocket support.
func TestIntegration_Mosquitto_WebSocket(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start Mosquitto with WebSocket
	mosquitto, err := NewMosquitto().
		EnableWebSocket().
		AllowAnonymous(true).
		ReadyTimeout(30 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start Mosquitto")
	defer mosquitto.Remove(ctx)

	// Verify WebSocket port is set
	assert.Greater(t, mosquitto.WebSocketPort(), 0, "WebSocket port should be set")
	assert.Contains(t, mosquitto.WebSocketURL(), "ws://127.0.0.1:")
}

// ═══════════════════════════════════════════════════════════════════════════
// LocalStack Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_LocalStack_Start validates LocalStack container startup.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	NewLocalStack() → WithSQS() → Start() → health check → Remove()
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_LocalStack_Start(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start LocalStack with SQS
	localstack, err := NewLocalStack().
		WithSQS().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start LocalStack")
	defer localstack.Remove(ctx)

	// Verify port is set
	assert.Greater(t, localstack.EdgePort(), 0, "Edge port should be set")

	// Verify health endpoint
	healthURL := fmt.Sprintf("http://localhost:%d/_localstack/health", localstack.EdgePort())
	resp, err := http.Get(healthURL)
	require.NoError(t, err, "Health check should succeed")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Health check should return 200")

	// Read and log the health response for debugging
	body, _ := io.ReadAll(resp.Body)
	t.Logf("LocalStack health: %s", string(body))
}

// TestIntegration_LocalStack_Services validates service configuration.
func TestIntegration_LocalStack_Services(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	localstack, err := NewLocalStack().
		WithSQS().
		WithDynamoDB().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	require.NoError(t, err)
	defer localstack.Remove(ctx)

	assert.True(t, localstack.HasService("sqs"))
	assert.True(t, localstack.HasService("dynamodb"))
	assert.False(t, localstack.HasService("s3"))
}

// ═══════════════════════════════════════════════════════════════════════════
// DynamoDB Integration Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_DynamoDB_Start validates DynamoDB Local container startup.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────────
//
//	NewDynamoDB() → Start() → Endpoint() → HTTP request → Remove()
//
// ───────────────────────────────────────────────────────────────────────────
func TestIntegration_DynamoDB_Start(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start DynamoDB Local
	dynamodb, err := NewDynamoDB().
		InMemory(true).
		SharedDb(true).
		ReadyTimeout(30 * time.Second).
		Start(ctx)
	require.NoError(t, err, "Failed to start DynamoDB Local")
	defer dynamodb.Remove(ctx)

	// Verify port is set
	assert.Greater(t, dynamodb.Port(), 0, "Port should be set")

	// Verify endpoint format
	assert.Contains(t, dynamodb.Endpoint(), "http://localhost:")

	// Make a simple request to verify DynamoDB is responding
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("POST", dynamodb.Endpoint(), nil)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810.ListTables")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")

	resp, err := client.Do(req)
	require.NoError(t, err, "DynamoDB should respond")
	defer resp.Body.Close()

	// 400 is expected for empty body, but it means DynamoDB is running
	assert.True(t, resp.StatusCode == 200 || resp.StatusCode == 400,
		"DynamoDB should respond with 200 or 400, got %d", resp.StatusCode)
}

// ═══════════════════════════════════════════════════════════════════════════
// Cleanup Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_OrphanCleanup validates orphan detection and removal.
func TestIntegration_OrphanCleanup(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start a container
	container, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err)

	containerID := container.ID

	// Verify container is running
	assert.True(t, container.IsRunning(ctx))

	// Clean up the current session
	err = CleanupSession(ctx)
	require.NoError(t, err)

	// Verify container was removed
	cli := NewDockerCLI()
	_, err = cli.Run(ctx, "inspect", containerID)
	assert.Error(t, err, "Container should have been removed by cleanup")
}

// TestIntegration_CleanupService validates service-specific cleanup.
func TestIntegration_CleanupService(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	// Start two containers with different service types
	container1, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		ServiceType("service-a").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err)
	defer container1.Remove(ctx)

	container2, err := NewContainerBuilder().
		Image("alpine:latest").
		Cmd("sleep", "300").
		ServiceType("service-b").
		ReadyTimeout(10 * time.Second).
		Start(ctx)
	require.NoError(t, err)
	defer container2.Remove(ctx)

	// Clean up only service-a
	err = CleanupService(ctx, "service-a")
	require.NoError(t, err)

	// Verify service-a container was removed
	cli := NewDockerCLI()
	_, err = cli.Run(ctx, "inspect", container1.ID)
	assert.Error(t, err, "Container with service-a should have been removed")

	// Verify service-b container is still running
	assert.True(t, container2.IsRunning(ctx), "Container with service-b should still be running")
}
