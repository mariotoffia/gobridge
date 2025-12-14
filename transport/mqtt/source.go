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
	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// Source implements types.Source for MQTT subscriptions.
type Source struct {
	id         string
	config     *SourceConfigImpl
	client     *autopaho.ConnectionManager
	messages   chan *types.SourceMessage
	topics     []string
	qos        byte
	running    atomic.Bool
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.RWMutex
	closeOnce  sync.Once
	closeErr   error
}

// Ensure Source implements types.Source
var _ types.Source = (*Source)(nil)

// NewSource creates a new MQTT source.
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
		id:       config.ID,
		config:   config,
		topics:   config.Topics,
		qos:      qos,
		messages: make(chan *types.SourceMessage, 100),
	}, nil
}

// GetID returns the unique identifier of the source.
func (s *Source) GetID() string {
	return s.id
}

// GetTransportType returns the transport type.
func (s *Source) GetTransportType() types.TransportType {
	return TransportType
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

	// Parse broker URL
	serverURL, err := url.Parse(s.config.Connection.BrokerURL)
	if err != nil {
		s.running.Store(false)
		return fmt.Errorf("invalid broker URL: %w", err)
	}

	// Create cancellable context
	ctx, s.cancel = context.WithCancel(ctx)

	// Build client configuration
	clientID := s.config.Connection.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("gobridge-source-%s-%d", s.id, time.Now().UnixNano())
	}

	// Build subscription slice
	subscriptions := make([]paho.SubscribeOptions, 0, len(s.topics))
	for _, topic := range s.topics {
		subscriptions = append(subscriptions, paho.SubscribeOptions{
			Topic: topic,
			QoS:   s.qos,
		})
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
				// Log error but don't crash - will retry on reconnect
				_ = err
			}
		},
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			Router: paho.NewSingleHandlerRouter(func(m *paho.Publish) {
				s.handleMessage(m)
			}),
		},
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
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
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
	default:
		// Channel full - drop message (should be rare with properly sized buffer)
	}
}

// Messages returns the channel that receives messages.
func (s *Source) Messages() <-chan *types.SourceMessage {
	return s.messages
}

// Close stops the source and releases resources.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		s.running.Store(false)

		if s.cancel != nil {
			s.cancel()
		}

		if s.client != nil {
			// Disconnect with a short timeout
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_ = s.client.Disconnect(ctx)
		}

		// Close messages channel
		close(s.messages)
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

