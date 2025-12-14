package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ============================================================================
// Mosquitto Container
// ============================================================================

// MosquittoContainer represents a running Mosquitto MQTT broker container.
type MosquittoContainer struct {
	*Container

	// mqttPort is the host port for MQTT connections.
	mqttPort int

	// wsPort is the host port for WebSocket connections (0 if not enabled).
	wsPort int

	// configFile is the temporary config file path.
	configFile string

	// cleanup removes the temporary config file.
	cleanup func()
}

// MQTTPort returns the host port for MQTT connections.
func (m *MosquittoContainer) MQTTPort() int {
	return m.mqttPort
}

// WebSocketPort returns the host port for WebSocket connections.
// Returns 0 if WebSocket is not enabled.
func (m *MosquittoContainer) WebSocketPort() int {
	return m.wsPort
}

// BrokerURL returns the MQTT broker URL (tcp://host:port).
func (m *MosquittoContainer) BrokerURL() string {
	return fmt.Sprintf("tcp://127.0.0.1:%d", m.mqttPort)
}

// WebSocketURL returns the WebSocket URL (ws://host:port).
// Returns empty string if WebSocket is not enabled.
func (m *MosquittoContainer) WebSocketURL() string {
	if m.wsPort == 0 {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d", m.wsPort)
}

// Remove stops and removes the container and cleans up temp files.
func (m *MosquittoContainer) Remove(ctx context.Context) error {
	err := m.Container.Remove(ctx)
	if m.cleanup != nil {
		m.cleanup()
	}
	return err
}

// ============================================================================
// MosquittoBuilder
// ============================================================================

// MosquittoBuilder configures a Mosquitto container.
type MosquittoBuilder struct {
	// image is the Mosquitto Docker image.
	image string

	// name is the container name.
	name string

	// mqttPort is the host port for MQTT (0 for random).
	mqttPort int

	// wsEnabled enables WebSocket support.
	wsEnabled bool

	// wsPort is the host port for WebSocket (0 for random).
	wsPort int

	// allowAnonymous allows connections without authentication.
	allowAnonymous bool

	// persistenceEnabled enables message persistence.
	persistenceEnabled bool

	// customConfig is the full custom config content (overrides generated).
	customConfig string

	// configFile is an external config file to mount.
	configFile string

	// readyTimeout is how long to wait for the broker to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewMosquitto creates a new MosquittoBuilder with sensible defaults.
func NewMosquitto() *MosquittoBuilder {
	return &MosquittoBuilder{
		image:          "eclipse-mosquitto:latest",
		mqttPort:       0, // Random port
		wsEnabled:      false,
		wsPort:         0,
		allowAnonymous: true,
		readyTimeout:   30 * time.Second,
		cli:            NewDockerCLI(),
	}
}

// Image sets the Mosquitto Docker image.
func (b *MosquittoBuilder) Image(image string) *MosquittoBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *MosquittoBuilder) Name(name string) *MosquittoBuilder {
	b.name = name
	return b
}

// MQTTPort sets the host port for MQTT connections.
// Use 0 for a random available port.
func (b *MosquittoBuilder) MQTTPort(port int) *MosquittoBuilder {
	b.mqttPort = port
	return b
}

// WebSocketPort sets the host port for WebSocket connections.
// Use 0 for a random available port.
func (b *MosquittoBuilder) WebSocketPort(port int) *MosquittoBuilder {
	b.wsPort = port
	b.wsEnabled = true
	return b
}

// EnableWebSocket enables WebSocket support with a random port.
func (b *MosquittoBuilder) EnableWebSocket() *MosquittoBuilder {
	b.wsEnabled = true
	b.wsPort = 0
	return b
}

// AllowAnonymous sets whether anonymous connections are allowed.
func (b *MosquittoBuilder) AllowAnonymous(allow bool) *MosquittoBuilder {
	b.allowAnonymous = allow
	return b
}

// Persistence enables or disables message persistence.
func (b *MosquittoBuilder) Persistence(enabled bool) *MosquittoBuilder {
	b.persistenceEnabled = enabled
	return b
}

// WithConfig sets custom config content (overrides generated config).
func (b *MosquittoBuilder) WithConfig(content string) *MosquittoBuilder {
	b.customConfig = content
	return b
}

// WithConfigFile mounts an external config file.
func (b *MosquittoBuilder) WithConfigFile(path string) *MosquittoBuilder {
	b.configFile = path
	return b
}

// ReadyTimeout sets how long to wait for the broker to be ready.
func (b *MosquittoBuilder) ReadyTimeout(d time.Duration) *MosquittoBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *MosquittoBuilder) WithCLI(cli *DockerCLI) *MosquittoBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the Mosquitto container.
func (b *MosquittoBuilder) Start(ctx context.Context) (*MosquittoContainer, error) {
	var cleanup func()
	var configPath string

	// Pick free ports if not specified
	mqttPort := b.mqttPort
	if mqttPort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick MQTT port: %w", err)
		}
		mqttPort = p
	}

	wsPort := b.wsPort
	if b.wsEnabled && wsPort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick WebSocket port: %w", err)
		}
		wsPort = p
	}

	// Prepare config file
	if b.configFile != "" {
		// Use provided config file
		abs, err := filepath.Abs(b.configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve config path: %w", err)
		}
		configPath = abs
	} else {
		// Generate config
		configContent := b.generateConfig()
		if b.customConfig != "" {
			configContent = b.customConfig
		}

		// Write to temp file
		tmp, err := os.CreateTemp("", "mosquitto-*.conf")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp config: %w", err)
		}
		if _, err := tmp.WriteString(configContent); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("failed to write config: %w", err)
		}
		tmp.Close()

		configPath = tmp.Name()
		cleanup = func() {
			os.Remove(configPath)
		}
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("mosquitto").
		Volume(configPath, "/mosquitto/config/mosquitto.conf").
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Add port mappings with explicit 127.0.0.1 binding
	builder.Port(mqttPort, 1883)
	if b.wsEnabled {
		builder.Port(wsPort, 9001)
	}

	// Simple TCP ready check
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		return waitForTCP(ctx, "127.0.0.1", mqttPort)
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	mc := &MosquittoContainer{
		Container:  container,
		mqttPort:   mqttPort,
		configFile: configPath,
		cleanup:    cleanup,
	}

	if b.wsEnabled {
		mc.wsPort = wsPort
	}

	return mc, nil
}

// generateConfig generates the Mosquitto configuration.
func (b *MosquittoBuilder) generateConfig() string {
	var config string

	// Listener for MQTT
	config += "listener 1883 0.0.0.0\n"
	config += "protocol mqtt\n"
	config += "\n"

	// WebSocket listener if enabled
	if b.wsEnabled {
		config += "listener 9001 0.0.0.0\n"
		config += "protocol websockets\n"
		config += "\n"
	}

	// Authentication
	if b.allowAnonymous {
		config += "allow_anonymous true\n"
	} else {
		config += "allow_anonymous false\n"
	}
	config += "\n"

	// Persistence
	if b.persistenceEnabled {
		config += "persistence true\n"
		config += "persistence_location /mosquitto/data/\n"
	} else {
		config += "persistence false\n"
	}
	config += "\n"

	// Logging
	config += "log_dest stdout\n"

	return config
}

// ============================================================================
// Helper Functions
// ============================================================================

// pickFreePort finds an available TCP port.
func pickFreePort() (int, error) {
	for i := 0; i < 5; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		addr := ln.Addr().(*net.TCPAddr)
		port := addr.Port
		ln.Close()
		if port > 0 {
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not find free port")
}

// waitForTCP waits until a TCP connection can be established.
func waitForTCP(ctx context.Context, host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	var d net.Dialer

	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ============================================================================
// Mosquitto Test Helpers
// ============================================================================

// DefaultMosquittoConfig returns a default configuration suitable for most tests.
func DefaultMosquittoConfig() *MosquittoBuilder {
	return NewMosquitto().
		AllowAnonymous(true)
}

// MosquittoWithWebSocket returns a configuration with WebSocket enabled.
func MosquittoWithWebSocket() *MosquittoBuilder {
	return NewMosquitto().
		AllowAnonymous(true).
		EnableWebSocket()
}
