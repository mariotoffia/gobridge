package mqtt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// MQTTConnection implements types.Connection for MQTT brokers.
// It provides a shared underlying connection that can be used by multiple
// Source and Target instances, enabling efficient resource usage.
//
// # Shared Connection Pattern
//
// Instead of each Source and Target creating their own TCP connection to the broker,
// MQTTConnection manages a single connection that is shared:
//
//	conn := mqtt.NewConnection(config)
//	conn.Start(ctx, nil)
//
//	// Both source and target share the same underlying MQTT client
//	source, _ := conn.CreateSource(ctx, sourceConfig)
//	target, _ := conn.CreateTarget(ctx, targetConfig)
//
// # Lifecycle
//
// The connection must be started before creating sources/targets:
//  1. Create connection with NewConnection()
//  2. Start connection with Start()
//  3. Create sources/targets with CreateSource()/CreateTarget()
//  4. When done, close sources/targets first (they don't close the client)
//  5. Close the connection with Close() to disconnect from broker
type MQTTConnection struct {
	id     string
	config *MQTTConnectionConfig
	client *autopaho.ConnectionManager

	// router handles incoming messages and dispatches to registered sources
	router *messageRouter

	running   atomic.Bool
	cancel    context.CancelFunc
	mu        sync.RWMutex
	closeOnce sync.Once
	closeErr  error
}

// MQTTConnectionConfig holds configuration for creating an MQTTConnection.
type MQTTConnectionConfig struct {
	// ID is the unique identifier for this connection.
	ID string `json:"id"`

	// Connection holds the MQTT connection settings.
	Connection ConnectionConfig `json:"connection"`
}

// Ensure MQTTConnectionConfig implements types.ConnectionConfig
var _ types.ConnectionConfig = (*MQTTConnectionConfig)(nil)

func (c *MQTTConnectionConfig) GetID() string {
	return c.ID
}

func (c *MQTTConnectionConfig) GetTransportType() types.TransportType {
	return TransportType
}

func (c *MQTTConnectionConfig) GetBridgeID() string {
	return "" // Can be extended if needed
}

// messageRouter routes incoming MQTT messages to registered source handlers.
type messageRouter struct {
	mu       sync.RWMutex
	handlers map[string]func(*paho.Publish) // sourceID -> handler
}

func newMessageRouter() *messageRouter {
	return &messageRouter{
		handlers: make(map[string]func(*paho.Publish)),
	}
}

func (r *messageRouter) register(sourceID string, handler func(*paho.Publish)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[sourceID] = handler
}

func (r *messageRouter) unregister(sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handlers, sourceID)
}

func (r *messageRouter) handleMessage(m *paho.Publish) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Dispatch to all registered handlers
	for _, handler := range r.handlers {
		handler(m)
	}
}

// Ensure MQTTConnection implements required interfaces
var (
	_ types.Connection     = (*MQTTConnection)(nil)
	_ types.SourceProvider = (*MQTTConnection)(nil)
	_ types.TargetProvider = (*MQTTConnection)(nil)
)

// NewConnection creates a new MQTT connection.
func NewConnection(config *MQTTConnectionConfig) (*MQTTConnection, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	if config.Connection.BrokerURL == "" {
		return nil, errors.New("broker URL is required")
	}

	return &MQTTConnection{
		id:     config.ID,
		config: config,
		router: newMessageRouter(),
	}, nil
}

// GetID returns the unique identifier of the connection.
func (c *MQTTConnection) GetID() string {
	return c.id
}

// GetTransportType returns the transport type.
func (c *MQTTConnection) GetTransportType() types.TransportType {
	return TransportType
}

// SourceProvider returns this connection as a SourceProvider.
func (c *MQTTConnection) SourceProvider() types.SourceProvider {
	return c
}

// TargetProvider returns this connection as a TargetProvider.
func (c *MQTTConnection) TargetProvider() types.TargetProvider {
	return c
}

// Start establishes the connection to the MQTT broker.
func (c *MQTTConnection) Start(ctx context.Context, override types.ConnectionConfig) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("connection already running")
	}

	// Apply override config if provided
	if override != nil {
		if mqttOverride, ok := override.(*MQTTConnectionConfig); ok {
			c.config = mqttOverride
		}
	}

	// Parse broker URL
	serverURL, err := url.Parse(c.config.Connection.BrokerURL)
	if err != nil {
		c.running.Store(false)
		return fmt.Errorf("invalid broker URL: %w", err)
	}

	// Create cancellable context
	ctx, c.cancel = context.WithCancel(ctx)

	// Build client configuration
	clientID := c.config.Connection.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("gobridge-conn-%s-%d", c.id, time.Now().UnixNano())
	}

	keepAlive := c.config.Connection.KeepAlive
	if keepAlive == 0 {
		keepAlive = 30
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{serverURL},
		KeepAlive:  keepAlive,
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			Router:   paho.NewSingleHandlerRouter(c.router.handleMessage),
		},
	}

	// Configure authentication
	if c.config.Connection.Username != "" {
		cliCfg.ConnectUsername = c.config.Connection.Username
		cliCfg.ConnectPassword = []byte(c.config.Connection.Password)
	}

	// Configure session
	cliCfg.CleanStartOnInitialConnection = c.config.Connection.CleanStart
	if c.config.Connection.SessionExpiryInterval > 0 {
		cliCfg.SessionExpiryInterval = c.config.Connection.SessionExpiryInterval
	}

	// Configure TLS
	if c.config.Connection.TLS != nil && c.config.Connection.TLS.Enable {
		tlsConfig, err := buildTLSConfig(c.config.Connection.TLS)
		if err != nil {
			c.running.Store(false)
			return fmt.Errorf("failed to build TLS config: %w", err)
		}
		cliCfg.TlsCfg = tlsConfig
	}

	// Create connection manager
	client, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		c.running.Store(false)
		return fmt.Errorf("failed to create MQTT client: %w", err)
	}

	c.client = client

	// Wait for initial connection
	connectTimeout := c.config.Connection.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := client.AwaitConnection(connectCtx); err != nil {
		c.client = nil
		c.running.Store(false)
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
	}

	return nil
}

// Capabilities returns the capabilities of this connection.
func (c *MQTTConnection) Capabilities(topics ...string) map[string]types.Capabilities {
	caps := types.Capabilities{}

	// MQTT supports both publishing and subscribing
	caps.AddType(types.CapabilityPublishAtLeastOnce)
	caps.AddType(types.CapabilityPublishAtMostOnce)
	caps.AddType(types.CapabilityPublishExactOnce)
	caps.AddType(types.CapabilityReceiveAtLeastOnce)
	caps.AddType(types.CapabilityReceiveAtMostOnce)
	caps.AddType(types.CapabilityReceiveExactOnce)
	caps.AddType(types.CapabilityNativeRetry)

	result := make(map[string]types.Capabilities)
	if len(topics) == 0 {
		result["*"] = caps
	} else {
		for _, topic := range topics {
			result[topic] = caps
		}
	}

	return result
}

// CreateSource creates a new Source that shares this connection's MQTT client.
func (c *MQTTConnection) CreateSource(ctx context.Context, config types.SourceConfig) (types.Source, error) {
	if !c.running.Load() {
		return nil, errors.New("connection not started")
	}

	mqttConfig, ok := config.(*SourceConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *mqtt.SourceConfigImpl, got %T", config)
	}

	source, err := NewSourceWithClient(mqttConfig, c.client, c.router)
	if err != nil {
		return nil, err
	}

	return source, nil
}

// CreateTarget creates a new Target that shares this connection's MQTT client.
func (c *MQTTConnection) CreateTarget(ctx context.Context, config types.TargetConfig) (types.Target, error) {
	if !c.running.Load() {
		return nil, errors.New("connection not started")
	}

	mqttConfig, ok := config.(*TargetConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *mqtt.TargetConfigImpl, got %T", config)
	}

	target, err := NewTargetWithClient(mqttConfig, c.client)
	if err != nil {
		return nil, err
	}

	return target, nil
}

// Close disconnects from the broker and releases resources.
func (c *MQTTConnection) Close() error {
	c.closeOnce.Do(func() {
		c.running.Store(false)

		if c.cancel != nil {
			c.cancel()
		}

		if c.client != nil {
			// Disconnect with a short timeout
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			c.closeErr = c.client.Disconnect(ctx)
		}
	})

	return c.closeErr
}

// Client returns the underlying MQTT client for advanced use cases.
// Use with caution - prefer using CreateSource/CreateTarget instead.
func (c *MQTTConnection) Client() *autopaho.ConnectionManager {
	return c.client
}

// IsRunning returns true if the connection is currently active.
func (c *MQTTConnection) IsRunning() bool {
	return c.running.Load()
}
