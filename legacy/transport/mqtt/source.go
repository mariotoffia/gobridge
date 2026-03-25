package mqtt

import (
	"context"
	"crypto/tls"
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

// singleHandlerRouter is a simple paho.Router that routes all messages to a single handler.
type singleHandlerRouter struct {
	handler func(*paho.Publish)
	debug   log.Logger
}

// Ensure singleHandlerRouter implements paho.Router
var _ paho.Router = (*singleHandlerRouter)(nil)

// newSingleHandlerRouter creates a new singleHandlerRouter with the given handler.
func newSingleHandlerRouter(handler func(*paho.Publish)) *singleHandlerRouter {
	return &singleHandlerRouter{
		handler: handler,
		debug:   log.NOOPLogger{},
	}
}

// Route implements paho.Router interface.
func (r *singleHandlerRouter) Route(pb *packets.Publish) {
	if r.handler != nil {
		r.debug.Printf("routing message on topic %s", pb.Topic)
		// Convert packets.Publish to paho.Publish
		r.handler(packetToPublish(pb))
	}
}

// RegisterHandler implements paho.Router interface (no-op).
func (r *singleHandlerRouter) RegisterHandler(topic string, handler paho.MessageHandler) {}

// UnregisterHandler implements paho.Router interface (no-op).
func (r *singleHandlerRouter) UnregisterHandler(topic string) {}

// SetDebugLogger implements paho.Router interface.
func (r *singleHandlerRouter) SetDebugLogger(l log.Logger) {
	r.debug = l
}

// packetToPublish converts packets.Publish to paho.Publish.
func packetToPublish(pb *packets.Publish) *paho.Publish {
	return &paho.Publish{
		QoS:        pb.QoS,
		Retain:     pb.Retain,
		Topic:      pb.Topic,
		PacketID:   pb.PacketID,
		Payload:    pb.Payload,
		Properties: publishPropertiesFromPacket(pb.Properties),
	}
}

// publishPropertiesFromPacket converts packets.Properties to paho.PublishProperties.
func publishPropertiesFromPacket(p *packets.Properties) *paho.PublishProperties {
	if p == nil {
		return nil
	}
	props := &paho.PublishProperties{
		CorrelationData: p.CorrelationData,
		ContentType:     p.ContentType,
		ResponseTopic:   p.ResponseTopic,
		MessageExpiry:   p.MessageExpiry,
	}
	if len(p.User) > 0 {
		props.User = make([]paho.UserProperty, len(p.User))
		for i, u := range p.User {
			props.User[i] = paho.UserProperty{Key: u.Key, Value: u.Value}
		}
	}
	return props
}

// Source implements types.Source for MQTT subscriptions.
type Source struct {
	id        string
	config    *SourceConfigImpl
	client    *autopaho.ConnectionManager
	messages  chan *types.SourceMessage
	topics    []string
	qos       byte
	running   atomic.Bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.RWMutex
	closeOnce sync.Once
	closeErr  error

	// Log is the LogCreator for the source component (optional)
	Log types.LogCreator

	// sharedClient indicates the client is managed externally (by MQTTConnection)
	// and should NOT be closed when the Source is closed.
	sharedClient bool
	// router is used for shared client mode to register/unregister message handlers
	router *messageRouter
}

// Ensure Source implements types.Source
var _ types.Source = (*Source)(nil)

// NewSource creates a new MQTT source that manages its own connection.
// For shared connection mode, use NewSourceWithClient instead.
func NewSource(config *SourceConfigImpl) (*Source, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	if config.Connection.BrokerURL == "" {
		return nil, errors.New("broker URL is required")
	}
	if len(config.Topics) == 0 {
		return nil, errors.New("at least one topic is required")
	}

	qos := byte(config.QoS)
	if qos > 2 {
		qos = 1 // Default to QoS 1
	}

	return &Source{
		id:           config.ID,
		config:       config,
		topics:       config.Topics,
		qos:          qos,
		messages:     make(chan *types.SourceMessage, 100),
		sharedClient: false,
	}, nil
}

// NewSourceWithClient creates a new MQTT source that uses a shared client.
// The client is managed externally (typically by MQTTConnection) and will NOT
// be closed when the Source is closed.
//
// The router is used to register this source's message handler so it receives
// messages from the shared client.
//
// If loggerFactory is provided, the source will create its own LogCreator.
func NewSourceWithClient(config *SourceConfigImpl, client *autopaho.ConnectionManager, router *messageRouter, loggerFactory types.LoggerFactory) (*Source, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	if client == nil {
		return nil, errors.New("client is required for shared client mode")
	}
	if router == nil {
		return nil, errors.New("router is required for shared client mode")
	}
	if len(config.Topics) == 0 {
		return nil, errors.New("at least one topic is required")
	}

	qos := byte(config.QoS)
	if qos > 2 {
		qos = 1 // Default to QoS 1
	}

	s := &Source{
		id:           config.ID,
		config:       config,
		client:       client,
		topics:       config.Topics,
		qos:          qos,
		messages:     make(chan *types.SourceMessage, 100),
		sharedClient: true,
		router:       router,
	}

	// Create logger if factory is provided
	if loggerFactory != nil {
		s.Log = loggerFactory("mqtt-source:" + config.ID)
	}

	return s, nil
}

// GetID returns the unique identifier of the source.
func (s *Source) GetID() string {
	return s.id
}

// GetTransportType returns the transport type.
func (s *Source) GetTransportType() types.TransportType {
	return TransportType
}

// getTopics returns the topics this source is subscribed to.
// This is used internally for lifecycle coordination.
func (s *Source) getTopics() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topics
}

// Capabilities returns the capabilities of this source.
func (s *Source) Capabilities() types.Capabilities {
	caps := types.Capabilities{}

	// MQTT sources don't support redelivery via Nack
	// (once a message is delivered, it's delivered)

	switch s.qos {
	case 0:
		caps.AddType(types.CapabilityReceiveAtMostOnce)
	case 1:
		caps.AddType(types.CapabilityReceiveAtLeastOnce)
	case 2:
		caps.AddType(types.CapabilityReceiveExactOnce)
	}

	return caps
}

// Start begins receiving messages.
func (s *Source) Start(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("source already running")
	}

	if s.Log != nil {
		s.Log(ctx, types.LogLevelInfo).Int("topics", len(s.topics)).Int("qos", int(s.qos)).Msg("starting source")
	}

	// Create cancellable context
	ctx, s.cancel = context.WithCancel(ctx)

	// Build subscription slice
	subscriptions := make([]paho.SubscribeOptions, 0, len(s.topics))
	for _, topic := range s.topics {
		subscriptions = append(subscriptions, paho.SubscribeOptions{
			Topic: topic,
			QoS:   s.qos,
		})
	}

	if s.sharedClient {
		// Shared client mode - register handler and subscribe
		return s.startSharedMode(ctx, subscriptions)
	}

	// Standalone mode - create our own connection
	return s.startStandaloneMode(ctx, subscriptions)
}

// startSharedMode starts the source using a shared client from MQTTConnection.
func (s *Source) startSharedMode(ctx context.Context, subscriptions []paho.SubscribeOptions) error {
	// Register our message handler with the router
	s.router.register(s.id, s.handleMessage)

	if s.Log != nil {
		s.Log(ctx, types.LogLevelDebug).Msg("subscribing to topics")
	}

	// Subscribe to topics
	_, err := s.client.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: subscriptions,
	})
	if err != nil {
		s.router.unregister(s.id)
		s.running.Store(false)
		if s.Log != nil {
			s.Log(ctx, types.LogLevelError).Err(err).Msg("failed to subscribe to topics")
		}
		return fmt.Errorf("failed to subscribe to topics: %w", MapError(err))
	}

	if s.Log != nil {
		s.Log(ctx, types.LogLevelInfo).Msg("source started (shared mode)")
	}

	return nil
}

// startStandaloneMode starts the source with its own dedicated connection.
func (s *Source) startStandaloneMode(ctx context.Context, subscriptions []paho.SubscribeOptions) error {
	// Parse broker URL
	serverURL, err := url.Parse(s.config.Connection.BrokerURL)
	if err != nil {
		s.running.Store(false)
		return fmt.Errorf("invalid broker URL: %w", err)
	}

	// Build client configuration
	clientID := s.config.Connection.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("gobridge-source-%s-%d", s.id, time.Now().UnixNano())
	}

	// Build autopaho config
	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{serverURL},
		KeepAlive:  30,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			// Subscribe to topics on connection
			_, err := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: subscriptions,
			})
			if err != nil {
				if s.Log != nil {
					s.Log(ctx, types.LogLevelError).Err(err).Msg("failed to subscribe on reconnect")
				}
			}
		},
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			Router:   newSingleHandlerRouter(s.handleMessage),
		},
	}

	// Wire paho logging to our logger at debug level
	if s.Log != nil {
		pahoAdapter := NewPahoLoggerAdapter(s.Log, ctx)
		cliCfg.Debug = pahoAdapter
		cliCfg.Errors = pahoAdapter
	}

	// Configure authentication
	if s.config.Connection.Username != "" {
		cliCfg.ConnectUsername = s.config.Connection.Username
		cliCfg.ConnectPassword = []byte(s.config.Connection.Password)
	}

	// Configure session
	cliCfg.CleanStartOnInitialConnection = s.config.Connection.CleanStart
	if s.config.Connection.SessionExpiryInterval > 0 {
		cliCfg.SessionExpiryInterval = s.config.Connection.SessionExpiryInterval
	}

	// Configure TLS
	if s.config.Connection.TLS != nil && s.config.Connection.TLS.Enable {
		tlsConfig, err := buildTLSConfig(s.config.Connection.TLS)
		if err != nil {
			s.running.Store(false)
			return fmt.Errorf("failed to build TLS config: %w", err)
		}
		cliCfg.TlsCfg = tlsConfig
	}

	// Create connection manager
	client, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		s.running.Store(false)
		if s.Log != nil {
			s.Log(ctx, types.LogLevelError).Err(err).Msg("failed to create MQTT client")
		}
		return fmt.Errorf("failed to create MQTT client: %w", err)
	}

	s.client = client

	// Wait for initial connection
	connectTimeout := s.config.Connection.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := client.AwaitConnection(connectCtx); err != nil {
		s.client = nil
		s.running.Store(false)
		if s.Log != nil {
			s.Log(ctx, types.LogLevelError).Err(err).Msg("failed to connect to MQTT broker")
		}
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
	}

	if s.Log != nil {
		s.Log(ctx, types.LogLevelInfo).Str("clientID", clientID).Msg("source started (standalone mode)")
	}

	return nil
}

// handleMessage processes an incoming MQTT message.
func (s *Source) handleMessage(m *paho.Publish) {
	if !s.running.Load() {
		return
	}

	msg := types.Message{
		CreatedAt: time.Now(),
		Topic:     m.Topic,
		Payload:   m.Payload,
		Qos:       &types.QosLevel{Level: int(m.QoS)},
	}

	// Extract properties if available
	if m.Properties != nil {
		msg.Metadata = make(map[string]any)
		if m.Properties.CorrelationData != nil {
			msg.Metadata["correlationData"] = m.Properties.CorrelationData
		}
		if m.Properties.ResponseTopic != "" {
			msg.Metadata["responseTopic"] = m.Properties.ResponseTopic
		}
		if m.Properties.ContentType != "" {
			msg.Metadata["contentType"] = m.Properties.ContentType
		}
		if m.Properties.MessageExpiry != nil {
			msg.TTL = time.Duration(*m.Properties.MessageExpiry) * time.Second
		}
		if len(m.Properties.User) > 0 {
			userProps := make(map[string]string)
			for _, prop := range m.Properties.User {
				userProps[prop.Key] = prop.Value
			}
			msg.Metadata["userProperties"] = userProps
		}
	}

	srcMsg := &types.SourceMessage{
		Message: msg,
		Ack: func() error {
			// MQTT handles acknowledgment at protocol level based on QoS
			// For QoS 1/2, paho automatically sends PUBACK/PUBREC
			return nil
		},
		Nack: func(reason error) error {
			// MQTT doesn't support nack - message is already delivered
			// Log the failure reason but can't redeliver
			return nil
		},
		Extend: func(ctx context.Context) error {
			// MQTT doesn't have visibility timeout concept
			return nil
		},
	}

	// Send to channel (non-blocking with timeout)
	select {
	case s.messages <- srcMsg:
		if s.Log != nil {
			s.Log(context.Background(), types.LogLevelDebug).Str("topic", m.Topic).Int("payload_size", len(m.Payload)).Msg("message received")
		}
	default:
		// Channel full - drop message (should be rare with properly sized buffer)
		if s.Log != nil {
			s.Log(context.Background(), types.LogLevelWarn).Str("topic", m.Topic).Msg("message dropped (channel full)")
		}
	}
}

// Messages returns the channel that receives messages.
func (s *Source) Messages() <-chan *types.SourceMessage {
	return s.messages
}

// Close stops the source and releases resources.
// For shared client mode, this does NOT close the underlying MQTT connection -
// that is managed by the MQTTConnection that created this source.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		ctx := context.Background()
		if s.Log != nil {
			s.Log(ctx, types.LogLevelInfo).Msg("closing source")
		}

		s.running.Store(false)

		if s.cancel != nil {
			s.cancel()
		}

		if s.sharedClient {
			// Shared mode - unregister from router but don't close client
			if s.router != nil {
				s.router.unregister(s.id)
			}
			// Unsubscribe from topics (best effort)
			if s.client != nil {
				unsubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_, _ = s.client.Unsubscribe(unsubCtx, &paho.Unsubscribe{
					Topics: s.topics,
				})
			}
		} else {
			// Standalone mode - disconnect and close client
			if s.client != nil {
				disconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_ = s.client.Disconnect(disconnectCtx)
			}
		}

		// Close messages channel
		close(s.messages)

		if s.Log != nil {
			s.Log(ctx, types.LogLevelInfo).Msg("source closed")
		}
	})

	return s.closeErr
}

// buildTLSConfig creates a TLS configuration from TLSConfig.
func buildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	// Load CA certificate if provided
	if cfg.CACertFile != "" {
		// TODO: Load CA cert from file
	}

	// Load client certificate if provided
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}
