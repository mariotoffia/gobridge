// ═══════════════════════════════════════════════════════════════════════════
// Azure Service Bus Transport - Local Test Helper
//
// Provides helper functions for integration testing with Apache Artemis
// as an Azure Service Bus compatible test environment.
//
// Architecture:
// ┌─────────────────────────────────────────────────────────────────────────┐
// │  Test Code                                                              │
// │      │                                                                  │
// │      ▼                                                                  │
// │  ServiceBusLocalHelper                                                  │
// │      │                                                                  │
// │      ├─ ConnectionString()  → Azure SDK compatible connection string   │
// │      ├─ CreateQueue()       → Creates ANYCAST queue in Artemis         │
// │      ├─ CreateTopic()       → Creates MULTICAST address                │
// │      └─ CreateSubscription()→ Creates subscription queue               │
// │      │                                                                  │
// │      ▼                                                                  │
// │  ArtemisContainer (Docker)                                              │
// │      │                                                                  │
// │      └─ AMQP 1.0 (Azure SDK compatible)                                │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════
package servicebustests

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/credentials/builders"
	"github.com/mariotoffia/gobridge/tests/docker"
)

// ServiceBusLocalHelper wraps ArtemisContainer for test convenience.
// It provides methods for creating and managing Service Bus entities
// during integration tests.
type ServiceBusLocalHelper struct {
	container *docker.ArtemisContainer
	t         *testing.T

	// createdQueues tracks queues created during the test for cleanup.
	createdQueues []string

	// createdTopics tracks topics created during the test for cleanup.
	createdTopics []string
}

// NewServiceBusLocalHelper creates a new helper for Service Bus testing.
func NewServiceBusLocalHelper(t *testing.T, container *docker.ArtemisContainer) *ServiceBusLocalHelper {
	t.Helper()
	return &ServiceBusLocalHelper{
		container:     container,
		t:             t,
		createdQueues: make([]string, 0),
		createdTopics: make([]string, 0),
	}
}

// ConnectionString returns an Azure Service Bus compatible connection string.
// The Azure SDK parses this format, and Artemis ignores SAS token validation.
func (h *ServiceBusLocalHelper) ConnectionString() string {
	return h.container.ConnectionString()
}

// AMQPPort returns the host port for AMQP connections.
func (h *ServiceBusLocalHelper) AMQPPort() int {
	return h.container.AMQPPort()
}

// ConsoleURL returns the Artemis web console URL for debugging.
func (h *ServiceBusLocalHelper) ConsoleURL() string {
	return h.container.ConsoleURL()
}

// TLSCredentials returns the TLS credentials if TLS is enabled.
// Returns nil if TLS is not enabled on the container.
func (h *ServiceBusLocalHelper) TLSCredentials() *builders.SelfSignedResult {
	return h.container.TLSCredentials()
}

// IsTLSEnabled returns true if TLS is enabled on this container.
func (h *ServiceBusLocalHelper) IsTLSEnabled() bool {
	return h.container.IsTLSEnabled()
}

// TLSConfig returns a tls.Config configured with the container's CA certificate.
// Returns nil if TLS is not enabled.
// Use this with the Azure SDK client options.
func (h *ServiceBusLocalHelper) TLSConfig() *tls.Config {
	tlsCreds := h.container.TLSCredentials()
	if tlsCreds == nil {
		return nil
	}

	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM([]byte(tlsCreds.CAPEM))

	return &tls.Config{
		RootCAs: certPool,
	}
}

// CreateQueue creates an ANYCAST queue in Artemis.
// ANYCAST queues behave like Azure Service Bus queues (point-to-point).
// Returns the queue name for use in configuration.
func (h *ServiceBusLocalHelper) CreateQueue(ctx context.Context, name string) string {
	h.t.Helper()

	err := h.container.CreateQueue(ctx, name)
	if err != nil {
		h.t.Fatalf("failed to create queue %s: %v", name, err)
	}

	h.createdQueues = append(h.createdQueues, name)
	return name
}

// CreateTopic creates a MULTICAST address in Artemis.
// MULTICAST addresses behave like Azure Service Bus topics (pub-sub).
func (h *ServiceBusLocalHelper) CreateTopic(ctx context.Context, name string) {
	h.t.Helper()

	err := h.container.CreateTopic(ctx, name)
	if err != nil {
		h.t.Fatalf("failed to create topic %s: %v", name, err)
	}

	h.createdTopics = append(h.createdTopics, name)
}

// CreateSubscription creates a subscription queue on a topic.
// This creates a MULTICAST queue bound to the topic address.
func (h *ServiceBusLocalHelper) CreateSubscription(ctx context.Context, topic, subscription string) {
	h.t.Helper()

	err := h.container.CreateSubscription(ctx, topic, subscription)
	if err != nil {
		h.t.Fatalf("failed to create subscription %s on topic %s: %v", subscription, topic, err)
	}
}

// Cleanup removes all created queues. Call this in a defer block.
func (h *ServiceBusLocalHelper) Cleanup(ctx context.Context) {
	// Best effort cleanup - don't fail the test if cleanup fails
	for _, queue := range h.createdQueues {
		_ = h.container.DeleteQueue(ctx, queue)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Test Setup Helpers
// ═══════════════════════════════════════════════════════════════════════════

// SetupServiceBusTest creates an Artemis container and helper for testing.
// Returns the helper and a cleanup function.
//
// Usage:
//
//	helper, cleanup := SetupServiceBusTest(t)
//	defer cleanup()
//	// ... test code using helper
func SetupServiceBusTest(t *testing.T) (*ServiceBusLocalHelper, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.NewArtemis().
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis: %v", err)
	}

	helper := NewServiceBusLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		_ = container.Remove(ctx)
	}

	return helper, cleanup
}

// SetupServiceBusTestWithQueue creates an Artemis container with a pre-created queue.
// Returns the helper, queue name, and cleanup function.
//
// Usage:
//
//	helper, queueName, cleanup := SetupServiceBusTestWithQueue(t, "test-queue")
//	defer cleanup()
//	// ... test code using helper and queueName
func SetupServiceBusTestWithQueue(t *testing.T, queueName string) (*ServiceBusLocalHelper, string, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.NewArtemis().
		WithQueue(queueName).
		ReadyTimeout(90 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis with queue: %v", err)
	}

	helper := NewServiceBusLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		_ = container.Remove(ctx)
	}

	return helper, queueName, cleanup
}

// SetupServiceBusTestWithTopic creates an Artemis container with TLS and a pre-created topic and subscription.
// Returns the helper, topic name, subscription name, and cleanup function.
func SetupServiceBusTestWithTopic(t *testing.T, topicName, subscriptionName string) (*ServiceBusLocalHelper, string, string, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.NewArtemis().
		WithTLS().
		WithTopic(topicName).
		WithSubscription(topicName, subscriptionName).
		ReadyTimeout(120 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis with topic: %v", err)
	}

	helper := NewServiceBusLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		_ = container.Remove(ctx)
	}

	return helper, topicName, subscriptionName, cleanup
}

// SetupServiceBusTestWithTLS creates an Artemis container with TLS enabled.
// This is required for testing with the Azure SDK which always uses TLS.
// Returns the helper and a cleanup function.
//
// Usage:
//
//	helper, cleanup := SetupServiceBusTestWithTLS(t)
//	defer cleanup()
//	// Get TLS config for Azure SDK
//	tlsConfig := helper.TLSConfig()
//	// ... test code using helper and tlsConfig
func SetupServiceBusTestWithTLS(t *testing.T) (*ServiceBusLocalHelper, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.NewArtemis().
		WithTLS().
		ReadyTimeout(120 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis with TLS: %v", err)
	}

	helper := NewServiceBusLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		_ = container.Remove(ctx)
	}

	return helper, cleanup
}

// SetupServiceBusTestWithTLSAndQueue creates an Artemis container with TLS and a pre-created queue.
// Returns the helper, queue name, and cleanup function.
//
// Usage:
//
//	helper, queueName, cleanup := SetupServiceBusTestWithTLSAndQueue(t, "test-queue")
//	defer cleanup()
//	tlsConfig := helper.TLSConfig()
//	// ... test code
func SetupServiceBusTestWithTLSAndQueue(t *testing.T, queueName string) (*ServiceBusLocalHelper, string, func()) {
	t.Helper()
	docker.RequireDocker(t)

	ctx := context.Background()
	container, err := docker.NewArtemis().
		WithTLS().
		WithQueue(queueName).
		ReadyTimeout(120 * time.Second).
		Start(ctx)
	if err != nil {
		t.Fatalf("failed to start Artemis with TLS and queue: %v", err)
	}

	helper := NewServiceBusLocalHelper(t, container)

	cleanup := func() {
		helper.Cleanup(ctx)
		_ = container.Remove(ctx)
	}

	return helper, queueName, cleanup
}

// ═══════════════════════════════════════════════════════════════════════════
// Utility Functions
// ═══════════════════════════════════════════════════════════════════════════

// UniqueQueueName generates a unique queue name for testing.
// This helps avoid conflicts when running tests in parallel.
func UniqueQueueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// UniqueTopicName generates a unique topic name for testing.
func UniqueTopicName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
