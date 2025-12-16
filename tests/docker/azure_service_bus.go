package docker

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mariotoffia/gobridge/bridge/credentials/builders"
	"software.sslmate.com/src/go-pkcs12"
)

// ============================================================================
// Artemis Container (Azure Service Bus Test Environment)
// ============================================================================

// ArtemisContainer represents a running Apache ActiveMQ Artemis container
// configured for Azure Service Bus compatibility testing.
//
// # Azure Service Bus Compatibility
//
// Artemis supports AMQP 1.0 which the Azure Service Bus SDK uses. The SDK
// accepts connection strings where Artemis ignores the SAS token validation,
// allowing local testing without Azure credentials.
//
// # Supported Features
//
// Source operations:
//   - ReceiveMessages, CompleteMessage, AbandonMessage
//   - TTL expiry, ApplicationProperties, DLQ routing
//
// Target operations:
//   - SendMessage, SendBatch, ScheduleMessages
//   - Metadata mapping, Retry semantics
//
// # Known Differences from Azure Service Bus
//
//   - Sessions: No strict FIFO guarantee
//   - Lock renewal: Different timeout behavior
//   - Dead-letter reasons: Metadata differs
//   - Scheduled enqueue: Lower precision
type ArtemisContainer struct {
	*Container

	// amqpPort is the host port for AMQP connections (plain).
	amqpPort int

	// amqpsPort is the host port for AMQPS connections (TLS).
	amqpsPort int

	// consolePort is the host port for the web console.
	consolePort int

	// username is the broker username.
	username string

	// password is the broker password.
	password string

	// tlsCreds holds the generated TLS credentials (if TLS is enabled).
	tlsCreds *builders.SelfSignedResult

	// certDir is the temp directory containing TLS certificates (for cleanup).
	certDir string
}

// AMQPPort returns the host port for plain AMQP connections.
func (a *ArtemisContainer) AMQPPort() int {
	return a.amqpPort
}

// AMQPSPort returns the host port for AMQPS (TLS) connections.
// Returns 0 if TLS is not enabled.
func (a *ArtemisContainer) AMQPSPort() int {
	return a.amqpsPort
}

// TLSCredentials returns the generated TLS credentials for client configuration.
// Returns nil if TLS is not enabled.
func (a *ArtemisContainer) TLSCredentials() *builders.SelfSignedResult {
	return a.tlsCreds
}

// IsTLSEnabled returns true if TLS is enabled on this container.
func (a *ArtemisContainer) IsTLSEnabled() bool {
	return a.tlsCreds != nil
}

// ConsolePort returns the host port for the web console.
func (a *ArtemisContainer) ConsolePort() int {
	return a.consolePort
}

// ConsoleURL returns the web console URL.
func (a *ArtemisContainer) ConsoleURL() string {
	return fmt.Sprintf("http://localhost:%d/console", a.consolePort)
}

// ConnectionString returns an Azure Service Bus compatible connection string.
// The Azure SDK parses this format, and Artemis ignores SAS token validation.
// When TLS is enabled, this returns a connection string using the AMQPS port.
func (a *ArtemisContainer) ConnectionString() string {
	port := a.amqpPort
	if a.tlsCreds != nil {
		port = a.amqpsPort
	}
	return fmt.Sprintf(
		"Endpoint=sb://localhost:%d/;SharedAccessKeyName=%s;SharedAccessKey=%s;",
		port,
		a.username,
		a.password,
	)
}

// CreateQueue creates an ANYCAST queue in Artemis.
// ANYCAST queues behave like Azure Service Bus queues (point-to-point).
func (a *ArtemisContainer) CreateQueue(ctx context.Context, name string) error {
	_, err := a.Exec(ctx,
		"/var/lib/artemis-instance/bin/artemis", "queue", "create",
		"--name", name,
		"--address", name,
		"--anycast",
		"--auto-create-address",
		"--silent",
		"--user", a.username,
		"--password", a.password,
	)
	if err != nil {
		return fmt.Errorf("failed to create queue %s: %w", name, err)
	}
	return nil
}

// CreateTopic creates a MULTICAST address in Artemis.
// MULTICAST addresses behave like Azure Service Bus topics (pub-sub).
func (a *ArtemisContainer) CreateTopic(ctx context.Context, name string) error {
	_, err := a.Exec(ctx,
		"/var/lib/artemis-instance/bin/artemis", "address", "create",
		"--name", name,
		"--multicast",
		"--silent",
		"--user", a.username,
		"--password", a.password,
	)
	if err != nil {
		return fmt.Errorf("failed to create topic %s: %w", name, err)
	}
	return nil
}

// CreateSubscription creates a subscription queue on a topic.
// This creates a MULTICAST queue bound to the topic address.
func (a *ArtemisContainer) CreateSubscription(ctx context.Context, topic, subscription string) error {
	// First ensure the topic exists
	_ = a.CreateTopic(ctx, topic)

	// Create a queue with the subscription name bound to the topic
	_, err := a.Exec(ctx,
		"/var/lib/artemis-instance/bin/artemis", "queue", "create",
		"--name", subscription,
		"--address", topic,
		"--multicast",
		"--silent",
		"--user", a.username,
		"--password", a.password,
	)
	if err != nil {
		return fmt.Errorf("failed to create subscription %s on topic %s: %w", subscription, topic, err)
	}
	return nil
}

// DeleteQueue deletes a queue from Artemis.
func (a *ArtemisContainer) DeleteQueue(ctx context.Context, name string) error {
	_, err := a.Exec(ctx,
		"/var/lib/artemis-instance/bin/artemis", "queue", "delete",
		"--name", name,
		"--silent",
		"--user", a.username,
		"--password", a.password,
	)
	if err != nil {
		return fmt.Errorf("failed to delete queue %s: %w", name, err)
	}
	return nil
}

// ============================================================================
// ArtemisBuilder
// ============================================================================

// ArtemisBuilder configures an Artemis container for Azure Service Bus testing.
type ArtemisBuilder struct {
	// image is the Artemis Docker image.
	image string

	// name is the container name.
	name string

	// amqpPort is the host port for AMQP (0 for random).
	amqpPort int

	// amqpsPort is the host port for AMQPS/TLS (0 for random).
	amqpsPort int

	// consolePort is the host port for the web console (0 for random).
	consolePort int

	// username is the broker username.
	username string

	// password is the broker password.
	password string

	// enableTLS enables TLS on the AMQPS port.
	enableTLS bool

	// queues to create on startup.
	queues []string

	// topics to create on startup.
	topics []string

	// subscriptions maps topic -> []subscription names.
	subscriptions map[string][]string

	// readyTimeout is how long to wait for the broker to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewArtemis creates a new ArtemisBuilder with sensible defaults.
func NewArtemis() *ArtemisBuilder {
	return &ArtemisBuilder{
		image:         "apache/activemq-artemis:2.36.0",
		amqpPort:      0, // Random port
		consolePort:   0, // Random port
		username:      "artemis",
		password:      "artemis",
		queues:        []string{},
		topics:        []string{},
		subscriptions: make(map[string][]string),
		readyTimeout:  60 * time.Second,
		cli:           NewDockerCLI(),
	}
}

// Image sets the Artemis Docker image.
func (b *ArtemisBuilder) Image(image string) *ArtemisBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *ArtemisBuilder) Name(name string) *ArtemisBuilder {
	b.name = name
	return b
}

// AMQPPort sets the host port for AMQP connections.
// Use 0 for a random available port.
func (b *ArtemisBuilder) AMQPPort(port int) *ArtemisBuilder {
	b.amqpPort = port
	return b
}

// AMQPSPort sets the host port for AMQPS (TLS) connections.
// Use 0 for a random available port.
// This implicitly enables TLS.
func (b *ArtemisBuilder) AMQPSPort(port int) *ArtemisBuilder {
	b.amqpsPort = port
	b.enableTLS = true
	return b
}

// WithTLS enables TLS on the AMQPS port (5671 inside container).
// A self-signed certificate will be generated and mounted into the container.
// Use TLSCredentials() on the resulting container to get the CA for client configuration.
func (b *ArtemisBuilder) WithTLS() *ArtemisBuilder {
	b.enableTLS = true
	return b
}

// ConsolePort sets the host port for the web console.
// Use 0 for a random available port.
func (b *ArtemisBuilder) ConsolePort(port int) *ArtemisBuilder {
	b.consolePort = port
	return b
}

// Credentials sets the broker username and password.
func (b *ArtemisBuilder) Credentials(username, password string) *ArtemisBuilder {
	b.username = username
	b.password = password
	return b
}

// WithQueue adds a queue to create on startup.
func (b *ArtemisBuilder) WithQueue(name string) *ArtemisBuilder {
	b.queues = append(b.queues, name)
	return b
}

// WithQueues adds multiple queues to create on startup.
func (b *ArtemisBuilder) WithQueues(names ...string) *ArtemisBuilder {
	b.queues = append(b.queues, names...)
	return b
}

// WithTopic adds a topic to create on startup.
func (b *ArtemisBuilder) WithTopic(name string) *ArtemisBuilder {
	b.topics = append(b.topics, name)
	return b
}

// WithTopics adds multiple topics to create on startup.
func (b *ArtemisBuilder) WithTopics(names ...string) *ArtemisBuilder {
	b.topics = append(b.topics, names...)
	return b
}

// WithSubscription adds a subscription on a topic to create on startup.
func (b *ArtemisBuilder) WithSubscription(topic, subscription string) *ArtemisBuilder {
	b.subscriptions[topic] = append(b.subscriptions[topic], subscription)
	return b
}

// ReadyTimeout sets how long to wait for the broker to be ready.
func (b *ArtemisBuilder) ReadyTimeout(d time.Duration) *ArtemisBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *ArtemisBuilder) WithCLI(cli *DockerCLI) *ArtemisBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the Artemis container.
func (b *ArtemisBuilder) Start(ctx context.Context) (*ArtemisContainer, error) {
	// Pick free ports if not specified
	amqpPort := b.amqpPort
	if amqpPort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick AMQP port: %w", err)
		}
		amqpPort = p
	}

	consolePort := b.consolePort
	if consolePort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick console port: %w", err)
		}
		consolePort = p
	}

	// Prepare TLS if enabled
	var tlsCreds *builders.SelfSignedResult
	var certDir string
	var amqpsPort int

	if b.enableTLS {
		// Pick AMQPS port
		amqpsPort = b.amqpsPort
		if amqpsPort == 0 {
			p, err := pickFreePort()
			if err != nil {
				return nil, fmt.Errorf("failed to pick AMQPS port: %w", err)
			}
			amqpsPort = p
		}

		// Generate self-signed certificate
		var err error
		tlsCreds, err = builders.GenerateSelfSigned(builders.SelfSignedOptions{
			CommonName:  "localhost",
			DNSNames:    []string{"localhost"},
			IPAddresses: []string{"127.0.0.1"},
			ValidFor:    24 * time.Hour,
			IsCA:        true,
			KeyType:     "rsa",
			KeySize:     2048,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to generate TLS certificate: %w", err)
		}

		// Create temp directory for certificates and config
		certDir, err = os.MkdirTemp("", "artemis-tls-*")
		if err != nil {
			return nil, fmt.Errorf("failed to create cert directory: %w", err)
		}

		// Create subdirectories for certificates and config override
		etcDir := filepath.Join(certDir, "ssl")
		etcOverrideDir := filepath.Join(certDir, "etc-override")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to create ssl directory: %w", err)
		}
		if err := os.MkdirAll(etcOverrideDir, 0755); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to create etc-override directory: %w", err)
		}

		// Write PEM files
		certPath := filepath.Join(certDir, "server.pem")
		keyPath := filepath.Join(certDir, "server-key.pem")

		if err := os.WriteFile(certPath, []byte(tlsCreds.CertPEM), 0644); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to write certificate: %w", err)
		}
		if err := os.WriteFile(keyPath, []byte(tlsCreds.KeyPEM), 0600); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to write key: %w", err)
		}

		// Create PKCS12 keystore for Artemis
		keystorePath := filepath.Join(etcDir, "broker.p12")
		keystorePassword := "changeit"
		if err := createPKCS12Keystore(tlsCreds.CertPEM, tlsCreds.KeyPEM, keystorePath, keystorePassword); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to create keystore: %w", err)
		}

		// Create broker.xml with AMQPS acceptor
		brokerXML := generateBrokerXMLWithTLS(keystorePassword)
		brokerXMLPath := filepath.Join(etcOverrideDir, "broker.xml")
		if err := os.WriteFile(brokerXMLPath, []byte(brokerXML), 0644); err != nil {
			os.RemoveAll(certDir)
			return nil, fmt.Errorf("failed to write broker.xml: %w", err)
		}
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("artemis").
		Port(amqpPort, 5672).
		Port(consolePort, 8161).
		Env("AMQ_USER", b.username).
		Env("AMQ_PASSWORD", b.password).
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Configure TLS if enabled
	if b.enableTLS {
		builder.Port(amqpsPort, 5671)
		// Mount the keystore and broker.xml override
		builder.Volume(filepath.Join(certDir, "ssl"), "/opt/ssl")
		builder.Volume(filepath.Join(certDir, "etc-override"), "/var/lib/artemis-instance/etc-override")
		// Enable anonymous access for testing
		builder.Env("AMQ_EXTRA_ARGS", "--relax-jolokia --allow-anonymous")
	} else {
		builder.Env("AMQ_EXTRA_ARGS", "--relax-jolokia")
	}

	// Ready check via TCP on appropriate port
	readyPort := amqpPort
	if b.enableTLS {
		readyPort = amqpsPort
	}
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		return waitForTCP(ctx, "127.0.0.1", readyPort)
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		if certDir != "" {
			os.RemoveAll(certDir)
		}
		return nil, err
	}

	ac := &ArtemisContainer{
		Container:   container,
		amqpPort:    amqpPort,
		amqpsPort:   amqpsPort,
		consolePort: consolePort,
		username:    b.username,
		password:    b.password,
		tlsCreds:    tlsCreds,
		certDir:     certDir,
	}

	// Wait a bit for Artemis to fully initialize internal services
	// The TCP port may be open before the broker is ready to accept commands
	time.Sleep(2 * time.Second)

	// Create pre-configured queues
	for _, queue := range b.queues {
		if err := ac.CreateQueue(ctx, queue); err != nil {
			_ = ac.Remove(ctx)
			return nil, fmt.Errorf("failed to create queue %s: %w", queue, err)
		}
	}

	// Create pre-configured topics
	for _, topic := range b.topics {
		if err := ac.CreateTopic(ctx, topic); err != nil {
			_ = ac.Remove(ctx)
			return nil, fmt.Errorf("failed to create topic %s: %w", topic, err)
		}
	}

	// Create pre-configured subscriptions
	for topic, subs := range b.subscriptions {
		for _, sub := range subs {
			if err := ac.CreateSubscription(ctx, topic, sub); err != nil {
				_ = ac.Remove(ctx)
				return nil, fmt.Errorf("failed to create subscription %s on topic %s: %w", sub, topic, err)
			}
		}
	}

	return ac, nil
}

// ============================================================================
// Artemis Test Helpers
// ============================================================================

// DefaultArtemisConfig returns a default configuration suitable for most tests.
func DefaultArtemisConfig() *ArtemisBuilder {
	return NewArtemis()
}

// ArtemisForServiceBus returns a configuration optimized for Azure Service Bus testing.
// This pre-creates a test queue for immediate use.
func ArtemisForServiceBus() *ArtemisBuilder {
	return NewArtemis().
		WithQueue("test-queue")
}

// ArtemisWithTopics returns a configuration with topic/subscription support.
func ArtemisWithTopics() *ArtemisBuilder {
	return NewArtemis().
		WithTopic("test-topic").
		WithSubscription("test-topic", "test-subscription")
}

// ArtemisForServiceBusTLS returns a configuration optimized for Azure Service Bus testing with TLS.
// This pre-creates a test queue and enables TLS for Azure SDK compatibility.
func ArtemisForServiceBusTLS() *ArtemisBuilder {
	return NewArtemis().
		WithTLS().
		WithQueue("test-queue")
}

// ============================================================================
// TLS Helpers
// ============================================================================

// generateBrokerXMLWithTLS creates a complete broker.xml with AMQPS acceptor.
// This is mounted to etc-override to add SSL support to the Artemis broker.
func generateBrokerXMLWithTLS(keystorePassword string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<configuration xmlns="urn:activemq" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="urn:activemq /schema/artemis-configuration.xsd">
   <core xmlns="urn:activemq:core">
      <name>artemis</name>
      <persistence-enabled>true</persistence-enabled>
      <journal-type>NIO</journal-type>
      <paging-directory>data/paging</paging-directory>
      <bindings-directory>data/bindings</bindings-directory>
      <journal-directory>data/journal</journal-directory>
      <large-messages-directory>data/large-messages</large-messages-directory>
      <journal-datasync>true</journal-datasync>
      <journal-min-files>2</journal-min-files>
      <journal-pool-files>10</journal-pool-files>
      <journal-device-block-size>4096</journal-device-block-size>
      <journal-file-size>10M</journal-file-size>
      <journal-buffer-timeout>12000</journal-buffer-timeout>
      <journal-max-io>1</journal-max-io>
      <disk-scan-period>5000</disk-scan-period>
      <max-disk-usage>90</max-disk-usage>
      <critical-analyzer>true</critical-analyzer>
      <critical-analyzer-timeout>120000</critical-analyzer-timeout>
      <critical-analyzer-check-period>60000</critical-analyzer-check-period>
      <critical-analyzer-policy>HALT</critical-analyzer-policy>
      
      <!-- Disable security for testing -->
      <security-enabled>false</security-enabled>
      
      <acceptors>
         <!-- Standard AMQP acceptor (no TLS) on 5672 -->
         <acceptor name="amqp">tcp://0.0.0.0:5672?tcpSendBufferSize=1048576;tcpReceiveBufferSize=1048576;protocols=AMQP;useEpoll=true;amqpCredits=1000;amqpLowCredits=300;amqpDuplicateDetection=true</acceptor>
         
         <!-- AMQPS acceptor with TLS on 5671 -->
         <acceptor name="amqps">tcp://0.0.0.0:5671?tcpSendBufferSize=1048576;tcpReceiveBufferSize=1048576;protocols=AMQP;useEpoll=true;amqpCredits=1000;amqpLowCredits=300;amqpDuplicateDetection=true;sslEnabled=true;keyStorePath=/opt/ssl/broker.p12;keyStorePassword=%s</acceptor>
         
         <!-- Core protocol acceptor on 61616 -->
         <acceptor name="artemis">tcp://0.0.0.0:61616?tcpSendBufferSize=1048576;tcpReceiveBufferSize=1048576;protocols=CORE,AMQP,STOMP,HORNETQ,MQTT,OPENWIRE;useEpoll=true;amqpCredits=1000;amqpLowCredits=300;amqpMinLargeMessageSize=102400;amqpDuplicateDetection=true</acceptor>
      </acceptors>

      <address-settings>
         <address-setting match="#">
            <dead-letter-address>DLQ</dead-letter-address>
            <expiry-address>ExpiryQueue</expiry-address>
            <redelivery-delay>0</redelivery-delay>
            <max-size-bytes>-1</max-size-bytes>
            <message-counter-history-day-limit>10</message-counter-history-day-limit>
            <address-full-policy>PAGE</address-full-policy>
            <auto-create-queues>true</auto-create-queues>
            <auto-create-addresses>true</auto-create-addresses>
         </address-setting>
      </address-settings>

      <addresses>
         <address name="DLQ">
            <anycast>
               <queue name="DLQ"/>
            </anycast>
         </address>
         <address name="ExpiryQueue">
            <anycast>
               <queue name="ExpiryQueue"/>
            </anycast>
         </address>
      </addresses>
   </core>
</configuration>
`, keystorePassword)
}

// createPKCS12Keystore creates a PKCS12 keystore from PEM certificate and key.
// This is needed because Artemis (Java-based) uses PKCS12 keystores for TLS.
func createPKCS12Keystore(certPEM, keyPEM, outputPath, password string) error {
	// Parse certificate
	certBlock, _ := pem.Decode([]byte(certPEM))
	if certBlock == nil {
		return fmt.Errorf("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Parse private key
	keyBlock, _ := pem.Decode([]byte(keyPEM))
	if keyBlock == nil {
		return fmt.Errorf("failed to decode key PEM")
	}

	var privateKey interface{}
	switch keyBlock.Type {
	case "RSA PRIVATE KEY":
		privateKey, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	case "EC PRIVATE KEY":
		privateKey, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "PRIVATE KEY":
		privateKey, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	default:
		return fmt.Errorf("unsupported key type: %s", keyBlock.Type)
	}
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	// Encode to PKCS12 using Modern encoder (SHA256 + AES-256-CBC)
	pfxData, err := pkcs12.Modern.Encode(privateKey, cert, nil, password)
	if err != nil {
		return fmt.Errorf("failed to encode PKCS12: %w", err)
	}

	// Write to file
	if err := os.WriteFile(outputPath, pfxData, 0600); err != nil {
		return fmt.Errorf("failed to write PKCS12 file: %w", err)
	}

	return nil
}

// Remove stops and removes the container, cleaning up TLS certificates if present.
func (a *ArtemisContainer) Remove(ctx context.Context) error {
	err := a.Container.Remove(ctx)
	// Clean up cert directory if it exists
	if a.certDir != "" {
		os.RemoveAll(a.certDir)
	}
	return err
}
