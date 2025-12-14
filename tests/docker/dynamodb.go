package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ============================================================================
// DynamoDB Container
// ============================================================================

// DynamoDBContainer represents a running DynamoDB Local container.
type DynamoDBContainer struct {
	*Container

	// port is the host port for DynamoDB connections.
	port int
}

// Port returns the host port for DynamoDB connections.
func (d *DynamoDBContainer) Port() int {
	return d.port
}

// Endpoint returns the DynamoDB endpoint URL.
func (d *DynamoDBContainer) Endpoint() string {
	return fmt.Sprintf("http://localhost:%d", d.port)
}

// ============================================================================
// DynamoDBBuilder
// ============================================================================

// DynamoDBBuilder configures a DynamoDB Local container.
type DynamoDBBuilder struct {
	// image is the DynamoDB Local Docker image.
	image string

	// name is the container name.
	name string

	// port is the host port (0 for random).
	port int

	// sharedDb uses a single database file for all requests.
	sharedDb bool

	// inMemory runs DynamoDB in memory without persistence.
	inMemory bool

	// dataDir is the host directory for data persistence.
	dataDir string

	// readyTimeout is how long to wait for DynamoDB to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewDynamoDB creates a new DynamoDBBuilder with sensible defaults.
func NewDynamoDB() *DynamoDBBuilder {
	return &DynamoDBBuilder{
		image:        "amazon/dynamodb-local:latest",
		port:         0, // Random port
		sharedDb:     true,
		inMemory:     true,
		readyTimeout: 30 * time.Second,
		cli:          NewDockerCLI(),
	}
}

// Image sets the DynamoDB Local Docker image.
func (b *DynamoDBBuilder) Image(image string) *DynamoDBBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *DynamoDBBuilder) Name(name string) *DynamoDBBuilder {
	b.name = name
	return b
}

// Port sets the host port for DynamoDB connections.
// Use 0 for a random available port.
func (b *DynamoDBBuilder) Port(port int) *DynamoDBBuilder {
	b.port = port
	return b
}

// SharedDb enables shared database mode.
// When enabled, all clients share the same database file regardless of credentials.
func (b *DynamoDBBuilder) SharedDb(enabled bool) *DynamoDBBuilder {
	b.sharedDb = enabled
	return b
}

// InMemory enables in-memory mode (no persistence).
func (b *DynamoDBBuilder) InMemory(enabled bool) *DynamoDBBuilder {
	b.inMemory = enabled
	return b
}

// DataDir sets the host directory for data persistence.
// Automatically disables in-memory mode.
func (b *DynamoDBBuilder) DataDir(dir string) *DynamoDBBuilder {
	b.dataDir = dir
	b.inMemory = false
	return b
}

// ReadyTimeout sets how long to wait for DynamoDB to be ready.
func (b *DynamoDBBuilder) ReadyTimeout(d time.Duration) *DynamoDBBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *DynamoDBBuilder) WithCLI(cli *DockerCLI) *DynamoDBBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the DynamoDB Local container.
func (b *DynamoDBBuilder) Start(ctx context.Context) (*DynamoDBContainer, error) {
	// Build command args
	var cmdArgs []string
	cmdArgs = append(cmdArgs, "-jar", "DynamoDBLocal.jar")

	if b.sharedDb {
		cmdArgs = append(cmdArgs, "-sharedDb")
	}

	if b.inMemory {
		cmdArgs = append(cmdArgs, "-inMemory")
	} else if b.dataDir != "" {
		cmdArgs = append(cmdArgs, "-dbPath", "/data")
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("dynamodb").
		Port(b.port, 8000).
		Cmd(cmdArgs...).
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Data persistence
	if b.dataDir != "" {
		builder.Volume(b.dataDir, "/data")
	}

	// Add ready check that verifies DynamoDB is accepting connections
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		port := c.GetHostPort(8000)
		if port == 0 {
			return fmt.Errorf("DynamoDB port not mapped")
		}

		// Try a simple request to verify DynamoDB is ready
		url := fmt.Sprintf("http://localhost:%d", port)
		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Amz-Target", "DynamoDB_20120810.ListTables")
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		// DynamoDB Local returns 400 for missing body, but that means it's running
		if resp.StatusCode == 400 || resp.StatusCode == 200 {
			return nil
		}

		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status: %d - %s", resp.StatusCode, string(body))
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		return nil, err
	}

	return &DynamoDBContainer{
		Container: container,
		port:      container.GetHostPort(8000),
	}, nil
}

// ============================================================================
// DynamoDB Test Helpers
// ============================================================================

// DefaultDynamoDBConfig returns a default in-memory configuration.
func DefaultDynamoDBConfig() *DynamoDBBuilder {
	return NewDynamoDB().
		SharedDb(true).
		InMemory(true)
}

// DynamoDBWithPersistence returns a configuration with data persistence.
func DynamoDBWithPersistence(dataDir string) *DynamoDBBuilder {
	return NewDynamoDB().
		SharedDb(true).
		DataDir(dataDir)
}

