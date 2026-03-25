package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventGridStart(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewEventGrid().
		WithPullTopic("test-topic", "test-subscription").
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Verify container is running
	if !container.IsRunning(ctx) {
		t.Fatal("container should be running")
	}

	// Verify port is assigned
	if container.Port() == 0 {
		t.Error("port should be assigned")
	}

	t.Logf("Event Grid Emulator started on port %d", container.Port())
	t.Logf("Endpoint: %s", container.Endpoint())
}

func TestEventGridEndpoints(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewEventGrid().
		WithPullTopic("my-events", "consumer-1").
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Check endpoint URLs
	endpoint := container.Endpoint()
	if endpoint == "" {
		t.Error("endpoint should not be empty")
	}

	topicEndpoint := container.TopicEndpoint("my-events")
	if !strings.Contains(topicEndpoint, "my-events/api/events") {
		t.Errorf("topic endpoint should contain path, got: %s", topicEndpoint)
	}

	nsEndpoint := container.NamespaceTopicEndpoint("my-events")
	if !strings.Contains(nsEndpoint, "topics/my-events:publish") {
		t.Errorf("namespace topic endpoint should contain path, got: %s", nsEndpoint)
	}

	t.Logf("Topic endpoint: %s", topicEndpoint)
	t.Logf("Namespace topic endpoint: %s", nsEndpoint)
}

func TestEventGridConfig(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	config := &EventGridConfig{}
	config.AddPullTopic("topic1", "sub1", "sub2")
	config.AddPullTopic("topic2", "sub3")
	config.AddFilter("sub1", "Event.TypeA", "Event.TypeB")

	container, err := NewEventGrid().
		WithConfig(config).
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Verify config is accessible
	returnedConfig := container.Config()
	if returnedConfig == nil {
		t.Fatal("config should not be nil")
	}

	if len(returnedConfig.Topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(returnedConfig.Topics))
	}

	topics := container.Topics()
	if len(topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(topics))
	}

	t.Logf("Configured topics: %v", topics)
}

func TestEventGridPullDelivery(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use the helper for pull delivery
	container, err := EventGridForPullDelivery().
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Verify it's configured for pull delivery
	config := container.Config()
	if config == nil || config.Topics == nil {
		t.Fatal("config should have topics")
	}

	endpoints, ok := config.Topics["test-topic"]
	if !ok {
		t.Fatal("test-topic should exist")
	}

	hasPullEndpoint := false
	for _, ep := range endpoints {
		if strings.HasPrefix(ep, "pull://") {
			hasPullEndpoint = true
			break
		}
	}

	if !hasPullEndpoint {
		t.Error("should have a pull:// endpoint")
	}

	t.Logf("Pull delivery configured with endpoints: %v", endpoints)
}

func TestEventGridWithMultipleTopics(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use the helper for multiple topics
	container, err := EventGridWithMultipleTopics().
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	topics := container.Topics()
	if len(topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(topics))
	}

	t.Logf("Configured topics: %v", topics)
}

func TestEventGridWithFilters(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use the helper with filters
	container, err := EventGridWithFilters().
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	config := container.Config()
	if len(config.Filters) != 2 {
		t.Errorf("expected 2 filters, got %d", len(config.Filters))
	}

	t.Logf("Configured filters: %+v", config.Filters)
}

func TestEventGridPublishCloudEvent(t *testing.T) {
	RequireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := NewEventGrid().
		WithPullTopic("events", "my-subscription").
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Publish a CloudEvent to the namespace topic endpoint
	cloudEvent := map[string]interface{}{
		"specversion": "1.0",
		"type":        "test.event",
		"source":      "/test",
		"id":          "test-id-1",
		"data": map[string]string{
			"message": "hello world",
		},
	}

	eventJSON, err := json.Marshal([]interface{}{cloudEvent})
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}

	url := container.NamespaceTopicEndpoint("events")
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(eventJSON)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/cloudevents-batch+json")
	req.Header.Set("aeg-sas-key", "fake-key") // The emulator ignores this

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to publish event: %v", err)
	}
	defer resp.Body.Close()

	// The emulator should accept the event (2xx response)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Errorf("expected 2xx status, got %d", resp.StatusCode)
	}

	t.Logf("Published CloudEvent, status: %d", resp.StatusCode)
}

func TestEventGridPushDeliveryWithWebhook(t *testing.T) {
	RequireDocker(t)

	// Create a test webhook server to receive push events
	received := make(chan []byte, 1)
	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body = make([]byte, 1024)
			n, _ := r.Body.Read(body)
			body = body[:n]
		}

		select {
		case received <- body:
		default:
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer webhookServer.Close()

	// Note: For push delivery to work, the emulator needs to reach the webhook.
	// When running in Docker, we use host.docker.internal to reach the host machine.
	// However, httptest.Server binds to 127.0.0.1 which may not be reachable from Docker.
	// This test primarily verifies the configuration works.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Extract port from webhook server URL
	// webhookServer.URL is like "http://127.0.0.1:12345"
	parts := strings.Split(webhookServer.URL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected webhook URL format: %s", webhookServer.URL)
	}
	// Note: We're using the local httptest server, which may not be reachable
	// from inside Docker. This test verifies the configuration is valid.

	container, err := NewEventGrid().
		WithTopic("push-topic", "http://host.docker.internal:9999/test").
		ReadyTimeout(60 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Event Grid Emulator: %v", err)
	}
	defer container.Remove(ctx)

	// Verify push delivery is configured
	config := container.Config()
	endpoints, ok := config.Topics["push-topic"]
	if !ok {
		t.Fatal("push-topic should exist")
	}

	hasHttpEndpoint := false
	for _, ep := range endpoints {
		if strings.HasPrefix(ep, "http://") {
			hasHttpEndpoint = true
			break
		}
	}

	if !hasHttpEndpoint {
		t.Error("should have an http:// endpoint for push delivery")
	}

	t.Logf("Push delivery topic endpoint: %s", container.TopicEndpoint("push-topic"))
}

func TestEventGridConfigBuilder(t *testing.T) {
	// Test the EventGridConfig builder methods
	config := &EventGridConfig{}

	config.AddTopic("topic1", "http://webhook1", "http://webhook2")
	config.AddPullTopic("topic2", "sub1", "sub2")
	config.AddFilter("sub1", "Event.Type1")

	if len(config.Topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(config.Topics))
	}

	if len(config.Topics["topic1"]) != 2 {
		t.Errorf("expected 2 endpoints for topic1, got %d", len(config.Topics["topic1"]))
	}

	// Check pull subscriptions have correct prefix
	topic2Endpoints := config.Topics["topic2"]
	for _, ep := range topic2Endpoints {
		if !strings.HasPrefix(ep, "pull://") {
			t.Errorf("pull topic endpoints should have pull:// prefix, got: %s", ep)
		}
	}

	if len(config.Filters) != 1 {
		t.Errorf("expected 1 filter, got %d", len(config.Filters))
	}

	if config.Filters[0].Subscription != "sub1" {
		t.Errorf("expected filter subscription 'sub1', got %s", config.Filters[0].Subscription)
	}
}

func TestEventGridNoTopicsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Should fail if no topics are configured
	_, err := NewEventGrid().
		Start(ctx)

	if err == nil {
		t.Error("expected error when no topics are configured")
	}

	if !strings.Contains(err.Error(), "topic") {
		t.Errorf("error should mention topics, got: %v", err)
	}
}
