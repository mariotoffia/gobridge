package docker

import (
	"context"
	"testing"
	"time"
)

func TestArtemisStart(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewArtemis().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	// Verify container is running
	if !container.IsRunning(ctx) {
		t.Fatal("container should be running")
	}

	// Verify ports are assigned
	if container.AMQPPort() == 0 {
		t.Error("AMQP port should be assigned")
	}
	if container.ConsolePort() == 0 {
		t.Error("console port should be assigned")
	}

	t.Logf("Artemis started on AMQP port %d, console port %d",
		container.AMQPPort(), container.ConsolePort())
}

func TestArtemisConnectionString(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewArtemis().
		Credentials("testuser", "testpass").
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	connStr := container.ConnectionString()

	// Verify connection string format
	if connStr == "" {
		t.Fatal("connection string should not be empty")
	}

	// Should contain Azure Service Bus format elements
	expected := []string{
		"Endpoint=sb://localhost:",
		"SharedAccessKeyName=testuser",
		"SharedAccessKey=testpass",
	}

	for _, s := range expected {
		if !contains(connStr, s) {
			t.Errorf("connection string should contain %q, got: %s", s, connStr)
		}
	}

	t.Logf("Connection string: %s", connStr)
}

func TestArtemisCreateQueue(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewArtemis().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	// Create a queue
	err = container.CreateQueue(ctx, "my-test-queue")
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}

	t.Log("Queue 'my-test-queue' created successfully")
}

func TestArtemisWithPreCreatedQueue(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Start with pre-created queue
	container, err := NewArtemis().
		WithQueue("precreated-queue").
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis with queue: %v", err)
	}
	defer container.Remove(ctx)

	t.Logf("Artemis started with pre-created queue on port %d", container.AMQPPort())
}

func TestArtemisCreateTopic(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewArtemis().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	// Create a topic
	err = container.CreateTopic(ctx, "my-test-topic")
	if err != nil {
		t.Fatalf("failed to create topic: %v", err)
	}

	// Create a subscription on the topic
	err = container.CreateSubscription(ctx, "my-test-topic", "my-subscription")
	if err != nil {
		t.Fatalf("failed to create subscription: %v", err)
	}

	t.Log("Topic 'my-test-topic' with subscription 'my-subscription' created successfully")
}

func TestArtemisForServiceBus(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use the helper that pre-creates test-queue
	container, err := ArtemisForServiceBus().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	connStr := container.ConnectionString()
	t.Logf("Azure Service Bus compatible connection: %s", connStr)
	t.Logf("Web console: %s", container.ConsoleURL())
}

func TestArtemisWithTopicsHelper(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use the helper that pre-creates topic with subscription
	container, err := ArtemisWithTopics().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}
	defer container.Remove(ctx)

	t.Logf("Artemis with topics started on AMQP port %d", container.AMQPPort())
}

// contains checks if s contains substr
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
