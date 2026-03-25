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
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"
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

	// coordinator manages atomic source/target lifecycle changes
	coordinator *mqttLifecycleCoordinator

	// activeSources tracks currently active sources by ID
	activeSources map[string]types.Source
	// activeTargets tracks currently active targets by ID
	activeTargets map[string]types.Target

	// Log is the LogCreator for the connection component (optional)
	Log types.LogCreator

	running   atomic.Bool
	draining  atomic.Bool
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

	// TransportRetry configures retry behavior for infrastructure operations.
	// If nil, uses bridge defaults.
	//
	// This is for TRANSPORT RETRY (infrastructure failures like connect, subscribe, publish).
	// NOT to be confused with RetryPolicy which is for MESSAGE RETRY.
	TransportRetry *types.TransportRetryConfig `json:"transportRetry,omitempty"`

	// LoggerFactory creates component-bound LogCreators (optional).
	// If provided, the connection and its sources/targets will use it for logging.
	LoggerFactory types.LoggerFactory `json:"-"`
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

// GetTransportRetryConfig returns connection-specific transport retry configuration.
// Returns nil to use bridge defaults.
func (c *MQTTConnectionConfig) GetTransportRetryConfig() *types.TransportRetryConfig {
	return c.TransportRetry
}

// messageRouter routes incoming MQTT messages to registered source handlers.
// It implements paho.Router interface.
type messageRouter struct {
	mu       sync.RWMutex
	handlers map[string]func(*paho.Publish) // sourceID -> handler
	debug    log.Logger
}

// Ensure messageRouter implements paho.Router
var _ paho.Router = (*messageRouter)(nil)

func newMessageRouter() *messageRouter {
	return &messageRouter{
		handlers: make(map[string]func(*paho.Publish)),
		debug:    log.NOOPLogger{},
	}
}

func (r *messageRouter) register(sourceID string, handler func(*paho.Publish)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[sourceID] = handler
	r.debug.Printf("registered handler for source %s (total handlers: %d)", sourceID, len(r.handlers))
}

func (r *messageRouter) unregister(sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handlers, sourceID)
	r.debug.Printf("unregistered handler for source %s (remaining handlers: %d)", sourceID, len(r.handlers))
}

// Route implements paho.Router interface.
// It dispatches messages to all registered handlers.
func (r *messageRouter) Route(pb *packets.Publish) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.debug.Printf("routing message on topic %s to %d handlers", pb.Topic, len(r.handlers))

	// Convert packets.Publish to paho.Publish for handlers
	m := packetToPublish(pb)

	// Dispatch to all registered handlers
	for sourceID, handler := range r.handlers {
		r.debug.Printf("dispatching to handler %s", sourceID)
		handler(m)
	}
}

// RegisterHandler implements paho.Router interface (no-op for this router).
func (r *messageRouter) RegisterHandler(topic string, handler paho.MessageHandler) {}

// UnregisterHandler implements paho.Router interface (no-op for this router).
func (r *messageRouter) UnregisterHandler(topic string) {}

// SetDebugLogger implements paho.Router interface.
func (r *messageRouter) SetDebugLogger(l log.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debug = l
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

	conn := &MQTTConnection{
		id:            config.ID,
		config:        config,
		router:        newMessageRouter(),
		activeSources: make(map[string]types.Source),
		activeTargets: make(map[string]types.Target),
	}

	// Create logger if factory is provided
	if config.LoggerFactory != nil {
		conn.Log = config.LoggerFactory("mqtt-connection:" + config.ID)
	}

	conn.coordinator = newMQTTLifecycleCoordinator(conn)

	return conn, nil
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
			// Update logger if factory changed
			if mqttOverride.LoggerFactory != nil {
				c.Log = mqttOverride.LoggerFactory("mqtt-connection:" + c.id)
			}
		}
	}

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Str("broker", c.config.Connection.BrokerURL).Msg("connecting to MQTT broker")
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
			Router:   c.router,
		},
	}

	// Wire paho logging to our logger at debug level
	if c.Log != nil {
		pahoAdapter := NewPahoLoggerAdapter(c.Log, ctx)
		cliCfg.Debug = pahoAdapter
		cliCfg.Errors = pahoAdapter
		c.router.SetDebugLogger(pahoAdapter)
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
		if c.Log != nil {
			c.Log(ctx, types.LogLevelError).Err(err).Msg("failed to create MQTT client")
		}
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
		if c.Log != nil {
			c.Log(ctx, types.LogLevelError).Err(err).Msg("failed to connect to MQTT broker")
		}
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
	}

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Str("clientID", clientID).Msg("connected to MQTT broker")
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

	// Pass logger factory to source
	source, err := NewSourceWithClient(mqttConfig, c.client, c.router, c.config.LoggerFactory)
	if err != nil {
		return nil, err
	}

	// Register the source for lifecycle tracking
	c.registerSource(config.GetID(), source)

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Str("sourceID", config.GetID()).Msg("created source")
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

	// Pass logger factory to target
	target, err := NewTargetWithClient(mqttConfig, c.client, c.config.LoggerFactory)
	if err != nil {
		return nil, err
	}

	// Register the target for lifecycle tracking
	c.registerTarget(config.GetID(), target)

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Str("targetID", config.GetID()).Msg("created target")
	}

	return target, nil
}

// Close disconnects from the broker and releases resources.
func (c *MQTTConnection) Close() error {
	c.closeOnce.Do(func() {
		ctx := context.Background()
		if c.Log != nil {
			c.Log(ctx, types.LogLevelInfo).Msg("closing connection")
		}

		c.running.Store(false)

		if c.cancel != nil {
			c.cancel()
		}

		if c.client != nil {
			// Disconnect with a short timeout
			disconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			c.closeErr = c.client.Disconnect(disconnectCtx)
		}

		if c.Log != nil {
			c.Log(ctx, types.LogLevelInfo).Msg("connection closed")
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

// UpdateSettings applies new connection settings.
// If RequiresReconnect() is true, it drains and reconnects.
func (c *MQTTConnection) UpdateSettings(ctx context.Context, settings types.ConnectionSettingsConfig) error {
	mqttSettings, ok := settings.(*MQTTConnectionSettings)
	if !ok {
		return fmt.Errorf("invalid settings type: expected *MQTTConnectionSettings, got %T", settings)
	}

	// Check if we need to reconnect
	if c.config.Connection.RequiresReconnect(mqttSettings) {
		if c.Log != nil {
			c.Log(ctx, types.LogLevelInfo).Msg("settings require reconnect")
		}
		// Full reconnect needed
		if err := c.Drain(ctx); err != nil {
			return fmt.Errorf("failed to drain before reconnect: %w", err)
		}
		return c.reconnectWithSettings(ctx, mqttSettings)
	}

	// Non-disruptive update (apply what we can without reconnect)
	c.applySettings(mqttSettings)
	return nil
}

// reconnectWithSettings disconnects and reconnects with new settings.
func (c *MQTTConnection) reconnectWithSettings(ctx context.Context, settings *MQTTConnectionSettings) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Disconnect current client
	if c.client != nil {
		disconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.client.Disconnect(disconnectCtx)
		cancel()
	}

	// Apply new settings
	c.applySettings(settings)

	// Reconnect
	return c.startLocked(ctx)
}

// applySettings applies settings that don't require reconnect.
func (c *MQTTConnection) applySettings(settings *MQTTConnectionSettings) {
	// Update config with new settings
	// This is where we'd apply non-disruptive settings
	// For now, most MQTT settings require reconnect
}

// startLocked starts the connection (caller must hold mu).
func (c *MQTTConnection) startLocked(ctx context.Context) error {
	// Parse broker URLs
	var serverURLs []*url.URL
	brokerURLs := c.config.Connection.BrokerURLs
	if len(brokerURLs) == 0 && c.config.Connection.BrokerURL != "" {
		brokerURLs = []string{c.config.Connection.BrokerURL}
	}

	for _, urlStr := range brokerURLs {
		serverURL, err := url.Parse(urlStr)
		if err != nil {
			return fmt.Errorf("invalid broker URL %q: %w", urlStr, err)
		}
		serverURLs = append(serverURLs, serverURL)
	}

	if len(serverURLs) == 0 {
		return errors.New("no broker URLs configured")
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
		ServerUrls: serverURLs,
		KeepAlive:  keepAlive,
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			Router:   c.router,
		},
	}

	// Wire paho logging to our logger at debug level
	if c.Log != nil {
		pahoAdapter := NewPahoLoggerAdapter(c.Log, ctx)
		cliCfg.Debug = pahoAdapter
		cliCfg.Errors = pahoAdapter
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
			return fmt.Errorf("failed to build TLS config: %w", err)
		}
		cliCfg.TlsCfg = tlsConfig
	}

	// Create connection manager
	client, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
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
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
	}

	return nil
}

// LifecycleCoordinator returns the coordinator for atomic Source/Target operations.
func (c *MQTTConnection) LifecycleCoordinator() types.LifecycleCoordinator {
	return c.coordinator
}

// Drain stops accepting new work and waits for in-flight messages to complete.
func (c *MQTTConnection) Drain(ctx context.Context) error {
	if !c.draining.CompareAndSwap(false, true) {
		// Already draining, wait for completion
		return c.waitForDrain(ctx)
	}
	defer c.draining.Store(false)

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Msg("draining connection")
	}

	c.mu.RLock()
	sources := make([]types.Source, 0, len(c.activeSources))
	for _, src := range c.activeSources {
		sources = append(sources, src)
	}
	c.mu.RUnlock()

	// Drain all active sources
	for _, src := range sources {
		if drainable, ok := src.(types.Drainable); ok {
			if err := drainable.Drain(ctx, types.DrainOptions{WaitForInFlight: true}); err != nil {
				return fmt.Errorf("failed to drain source: %w", err)
			}
		}
	}

	if c.Log != nil {
		c.Log(ctx, types.LogLevelInfo).Msg("connection drained")
	}

	return nil
}

// waitForDrain waits for an ongoing drain to complete.
func (c *MQTTConnection) waitForDrain(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !c.draining.Load() {
				return nil
			}
		}
	}
}

// IsDraining returns true if the connection is currently draining.
func (c *MQTTConnection) IsDraining() bool {
	return c.draining.Load()
}

// registerSource adds a source to the active sources map.
func (c *MQTTConnection) registerSource(id string, src types.Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeSources[id] = src
}

// unregisterSource removes a source from the active sources map.
func (c *MQTTConnection) unregisterSource(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activeSources, id)
}

// registerTarget adds a target to the active targets map.
func (c *MQTTConnection) registerTarget(id string, tgt types.Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeTargets[id] = tgt
}

// unregisterTarget removes a target from the active targets map.
func (c *MQTTConnection) unregisterTarget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activeTargets, id)
}
