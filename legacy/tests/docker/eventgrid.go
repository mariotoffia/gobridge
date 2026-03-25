package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ============================================================================
// EventGrid Emulator Container
// ============================================================================

// EventGridContainer represents a running Event Grid Emulator container.
//
// # Overview
//
// The Workleap Event Grid Emulator provides a local environment for testing
// Azure Event Grid functionality without connecting to Azure. It supports both
// Push and Pull delivery models.
//
// # Push Delivery
//
// Events published to a topic are forwarded to configured webhook URLs:
//
//	config := EventGridConfig{
//	    Topics: map[string][]string{
//	        "my-topic": {"http://host.docker.internal:8080/webhook"},
//	    },
//	}
//
// # Pull Delivery
//
// Events published to a topic are stored for pull-based consumption:
//
//	config := EventGridConfig{
//	    Topics: map[string][]string{
//	        "my-topic": {"pull://my-subscription"},
//	    },
//	}
//
// # Authentication
//
// The emulator ignores authentication. Any AzureKeyCredential or fake SAS token
// will work for SDK clients.
//
// # Supported APIs
//
// Push delivery:
//   - POST /topic-name/api/events (EventGridEvents or CloudEvents)
//
// Pull delivery:
//   - POST /topics/topic-name:publish (CloudEvents only)
//   - POST /topics/topic-name/eventsubscriptions/sub-name:receive
//   - POST /topics/topic-name/eventsubscriptions/sub-name:acknowledge
//   - POST /topics/topic-name/eventsubscriptions/sub-name:release
//   - POST /topics/topic-name/eventsubscriptions/sub-name:reject
//
// # Known Differences from Azure Event Grid
//
//   - No Azure AD authentication support (use access key or SAS)
//   - No validation endpoint handshake required
//   - Messages stored in memory only (lost on restart)
//   - Retry behavior may differ slightly
type EventGridContainer struct {
	*Container

	// port is the host port for the Event Grid API.
	port int

	// config is the emulator configuration.
	config *EventGridConfig

	// configFile is the path to the temporary config file (for cleanup).
	configFile string
}

// Port returns the host port for the Event Grid API.
func (e *EventGridContainer) Port() int {
	return e.port
}

// Endpoint returns the base Event Grid endpoint URL.
func (e *EventGridContainer) Endpoint() string {
	return fmt.Sprintf("http://127.0.0.1:%d", e.port)
}

// TopicEndpoint returns the endpoint URL for publishing events to a topic.
// This is the Custom Topic format for EventGridEvents/CloudEvents push delivery.
func (e *EventGridContainer) TopicEndpoint(topicName string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/%s/api/events", e.port, topicName)
}

// NamespaceTopicEndpoint returns the endpoint URL for publishing CloudEvents
// to a namespace topic using the pull delivery model.
func (e *EventGridContainer) NamespaceTopicEndpoint(topicName string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/topics/%s:publish", e.port, topicName)
}

// Config returns the emulator configuration.
func (e *EventGridContainer) Config() *EventGridConfig {
	return e.config
}

// Topics returns the list of configured topic names.
func (e *EventGridContainer) Topics() []string {
	if e.config == nil || e.config.Topics == nil {
		return nil
	}
	topics := make([]string, 0, len(e.config.Topics))
	for topic := range e.config.Topics {
		topics = append(topics, topic)
	}
	return topics
}

// Remove stops and removes the container, cleaning up temporary files.
func (e *EventGridContainer) Remove(ctx context.Context) error {
	err := e.Container.Remove(ctx)
	// Clean up config file if it was generated
	if e.configFile != "" {
		os.Remove(e.configFile)
	}
	return err
}

// ============================================================================
// EventGridConfig
// ============================================================================

// EventGridConfig represents the Event Grid Emulator configuration.
type EventGridConfig struct {
	// Topics maps topic names to their delivery endpoints.
	// For push delivery: webhook URLs (e.g., "http://host.docker.internal:8080/webhook")
	// For pull delivery: subscription URLs (e.g., "pull://my-subscription")
	Topics map[string][]string `json:"Topics,omitempty"`

	// Filters defines event type filters for subscriptions.
	Filters []EventGridFilter `json:"Filters,omitempty"`
}

// EventGridFilter represents an event type filter for a subscription.
type EventGridFilter struct {
	// Subscription is the subscription name or webhook URL to filter.
	Subscription string `json:"Subscription"`

	// IncludedEventTypes is the list of event types to include.
	// If empty, all event types are delivered.
	IncludedEventTypes []string `json:"IncludedEventTypes,omitempty"`
}

// AddTopic adds a topic with the specified delivery endpoints.
func (c *EventGridConfig) AddTopic(name string, endpoints ...string) *EventGridConfig {
	if c.Topics == nil {
		c.Topics = make(map[string][]string)
	}
	c.Topics[name] = append(c.Topics[name], endpoints...)
	return c
}

// AddPullTopic adds a topic with pull delivery subscriptions.
func (c *EventGridConfig) AddPullTopic(name string, subscriptions ...string) *EventGridConfig {
	if c.Topics == nil {
		c.Topics = make(map[string][]string)
	}
	for _, sub := range subscriptions {
		c.Topics[name] = append(c.Topics[name], fmt.Sprintf("pull://%s", sub))
	}
	return c
}

// AddFilter adds an event type filter for a subscription.
func (c *EventGridConfig) AddFilter(subscription string, eventTypes ...string) *EventGridConfig {
	c.Filters = append(c.Filters, EventGridFilter{
		Subscription:       subscription,
		IncludedEventTypes: eventTypes,
	})
	return c
}

// ============================================================================
// EventGridBuilder
// ============================================================================

// EventGridBuilder configures an Event Grid Emulator container.
type EventGridBuilder struct {
	// image is the Event Grid Emulator Docker image.
	image string

	// name is the container name.
	name string

	// port is the host port for the API (0 for random).
	port int

	// config is the emulator configuration.
	config *EventGridConfig

	// configFile is an external config file to mount.
	configFile string

	// readyTimeout is how long to wait for the emulator to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewEventGrid creates a new EventGridBuilder with sensible defaults.
func NewEventGrid() *EventGridBuilder {
	return &EventGridBuilder{
		image:        "workleap/eventgridemulator:latest",
		port:         0, // Random port
		config:       &EventGridConfig{Topics: make(map[string][]string)},
		readyTimeout: 60 * time.Second,
		cli:          NewDockerCLI(),
	}
}

// Image sets the Event Grid Emulator Docker image.
func (b *EventGridBuilder) Image(image string) *EventGridBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *EventGridBuilder) Name(name string) *EventGridBuilder {
	b.name = name
	return b
}

// Port sets the host port for the Event Grid API.
// Use 0 for a random available port.
func (b *EventGridBuilder) Port(port int) *EventGridBuilder {
	b.port = port
	return b
}

// WithConfig sets the complete emulator configuration.
func (b *EventGridBuilder) WithConfig(config *EventGridConfig) *EventGridBuilder {
	b.config = config
	return b
}

// WithConfigFile mounts an external configuration file.
// This overrides any programmatically configured topics.
func (b *EventGridBuilder) WithConfigFile(path string) *EventGridBuilder {
	b.configFile = path
	return b
}

// WithTopic adds a topic with push delivery webhook endpoints.
func (b *EventGridBuilder) WithTopic(name string, webhooks ...string) *EventGridBuilder {
	b.config.AddTopic(name, webhooks...)
	return b
}

// WithPullTopic adds a topic with pull delivery subscriptions.
func (b *EventGridBuilder) WithPullTopic(name string, subscriptions ...string) *EventGridBuilder {
	b.config.AddPullTopic(name, subscriptions...)
	return b
}

// WithFilter adds an event type filter for a subscription.
func (b *EventGridBuilder) WithFilter(subscription string, eventTypes ...string) *EventGridBuilder {
	b.config.AddFilter(subscription, eventTypes...)
	return b
}

// ReadyTimeout sets how long to wait for the emulator to be ready.
func (b *EventGridBuilder) ReadyTimeout(d time.Duration) *EventGridBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *EventGridBuilder) WithCLI(cli *DockerCLI) *EventGridBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the Event Grid Emulator container.
func (b *EventGridBuilder) Start(ctx context.Context) (*EventGridContainer, error) {
	// Validate configuration
	if b.config == nil || len(b.config.Topics) == 0 {
		if b.configFile == "" {
			return nil, fmt.Errorf("at least one topic must be configured")
		}
	}

	// Pick free port if not specified
	port := b.port
	if port == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick port: %w", err)
		}
		port = p
	}

	// Prepare config file
	var configPath string
	var generatedConfig bool

	if b.configFile != "" {
		// Use provided config file
		configPath = b.configFile
	} else {
		// Generate config file
		configJSON, err := json.MarshalIndent(b.config, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal config: %w", err)
		}

		// Write to temp file
		tmp, err := os.CreateTemp("", "eventgrid-*.json")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp config: %w", err)
		}
		if _, err := tmp.Write(configJSON); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("failed to write config: %w", err)
		}
		tmp.Close()

		configPath = tmp.Name()
		generatedConfig = true
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("eventgrid").
		Port(port, 6500).
		Volume(configPath, "/app/appsettings.json").
		ExtraHost("host.docker.internal:host-gateway").
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Ready check - verify the emulator responds to requests
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		// Try to make a simple HTTP request to see if it's up
		// The emulator doesn't have a dedicated health endpoint,
		// so we check if it responds to any request
		url := fmt.Sprintf("http://127.0.0.1:%d/", port)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		// Any response (even 404) means the server is up
		return nil
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		if generatedConfig {
			os.Remove(configPath)
		}
		return nil, err
	}

	// Track the config file only if we generated it
	var trackedConfigFile string
	if generatedConfig {
		trackedConfigFile = configPath
	}

	return &EventGridContainer{
		Container:  container,
		port:       port,
		config:     b.config,
		configFile: trackedConfigFile,
	}, nil
}

// ============================================================================
// EventGrid Test Helpers
// ============================================================================

// DefaultEventGridConfig returns a builder with a default test topic.
func DefaultEventGridConfig() *EventGridBuilder {
	return NewEventGrid().
		WithPullTopic("test-topic", "test-subscription")
}

// EventGridForPushDelivery returns a builder configured for push delivery testing.
// The provided webhookPort is the port where your test webhook server is listening.
func EventGridForPushDelivery(webhookPort int) *EventGridBuilder {
	webhook := fmt.Sprintf("http://host.docker.internal:%d/webhook", webhookPort)
	return NewEventGrid().
		WithTopic("test-topic", webhook)
}

// EventGridForPullDelivery returns a builder configured for pull delivery testing.
func EventGridForPullDelivery() *EventGridBuilder {
	return NewEventGrid().
		WithPullTopic("test-topic", "test-subscription")
}

// EventGridWithMultipleTopics returns a builder with multiple topics configured.
func EventGridWithMultipleTopics() *EventGridBuilder {
	return NewEventGrid().
		WithPullTopic("topic-1", "subscription-1").
		WithPullTopic("topic-2", "subscription-2a", "subscription-2b")
}

// EventGridWithFilters returns a builder with event type filtering configured.
func EventGridWithFilters() *EventGridBuilder {
	return NewEventGrid().
		WithPullTopic("filtered-topic", "type-a-subscription", "type-b-subscription", "all-types-subscription").
		WithFilter("type-a-subscription", "TypeA.Event").
		WithFilter("type-b-subscription", "TypeB.Event")
}
