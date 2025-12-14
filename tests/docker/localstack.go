package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// LocalStack Container
// ============================================================================

// LocalStackContainer represents a running LocalStack container.
type LocalStackContainer struct {
	*Container

	// edgePort is the host port for the LocalStack edge service.
	edgePort int

	// services is the list of enabled AWS services.
	services []string
}

// EdgePort returns the host port for LocalStack's edge service.
func (l *LocalStackContainer) EdgePort() int {
	return l.edgePort
}

// Endpoint returns the LocalStack endpoint URL.
func (l *LocalStackContainer) Endpoint() string {
	return fmt.Sprintf("http://localhost:%d", l.edgePort)
}

// SQSEndpoint returns the SQS endpoint URL.
func (l *LocalStackContainer) SQSEndpoint() string {
	return l.Endpoint()
}

// DynamoDBEndpoint returns the DynamoDB endpoint URL.
func (l *LocalStackContainer) DynamoDBEndpoint() string {
	return l.Endpoint()
}

// S3Endpoint returns the S3 endpoint URL.
func (l *LocalStackContainer) S3Endpoint() string {
	return l.Endpoint()
}

// SNSEndpoint returns the SNS endpoint URL.
func (l *LocalStackContainer) SNSEndpoint() string {
	return l.Endpoint()
}

// Services returns the list of enabled services.
func (l *LocalStackContainer) Services() []string {
	return l.services
}

// HasService checks if a service is enabled.
func (l *LocalStackContainer) HasService(service string) bool {
	service = strings.ToLower(service)
	for _, s := range l.services {
		if strings.ToLower(s) == service {
			return true
		}
	}
	return false
}

// ============================================================================
// LocalStackBuilder
// ============================================================================

// LocalStackBuilder configures a LocalStack container.
type LocalStackBuilder struct {
	// image is the LocalStack Docker image.
	image string

	// name is the container name.
	name string

	// edgePort is the host port for the edge service (0 for random).
	edgePort int

	// services is the list of AWS services to enable.
	services []string

	// debug enables debug logging.
	debug bool

	// dataDir is the host directory for data persistence.
	dataDir string

	// env holds additional environment variables.
	env map[string]string

	// readyTimeout is how long to wait for LocalStack to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewLocalStack creates a new LocalStackBuilder with sensible defaults.
func NewLocalStack() *LocalStackBuilder {
	return &LocalStackBuilder{
		image:        "localstack/localstack:latest",
		edgePort:     0, // Random port
		services:     []string{},
		debug:        false,
		env:          make(map[string]string),
		readyTimeout: 60 * time.Second,
		cli:          NewDockerCLI(),
	}
}

// Image sets the LocalStack Docker image.
func (b *LocalStackBuilder) Image(image string) *LocalStackBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *LocalStackBuilder) Name(name string) *LocalStackBuilder {
	b.name = name
	return b
}

// EdgePort sets the host port for LocalStack's edge service.
// Use 0 for a random available port.
func (b *LocalStackBuilder) EdgePort(port int) *LocalStackBuilder {
	b.edgePort = port
	return b
}

// WithSQS enables SQS.
func (b *LocalStackBuilder) WithSQS() *LocalStackBuilder {
	b.services = append(b.services, "sqs")
	return b
}

// WithDynamoDB enables DynamoDB.
func (b *LocalStackBuilder) WithDynamoDB() *LocalStackBuilder {
	b.services = append(b.services, "dynamodb")
	return b
}

// WithS3 enables S3.
func (b *LocalStackBuilder) WithS3() *LocalStackBuilder {
	b.services = append(b.services, "s3")
	return b
}

// WithSNS enables SNS.
func (b *LocalStackBuilder) WithSNS() *LocalStackBuilder {
	b.services = append(b.services, "sns")
	return b
}

// WithKinesis enables Kinesis.
func (b *LocalStackBuilder) WithKinesis() *LocalStackBuilder {
	b.services = append(b.services, "kinesis")
	return b
}

// WithLambda enables Lambda.
func (b *LocalStackBuilder) WithLambda() *LocalStackBuilder {
	b.services = append(b.services, "lambda")
	return b
}

// WithServices enables the specified services.
func (b *LocalStackBuilder) WithServices(services ...string) *LocalStackBuilder {
	b.services = append(b.services, services...)
	return b
}

// Debug enables debug logging.
func (b *LocalStackBuilder) Debug(enabled bool) *LocalStackBuilder {
	b.debug = enabled
	return b
}

// DataDir sets the host directory for data persistence.
func (b *LocalStackBuilder) DataDir(dir string) *LocalStackBuilder {
	b.dataDir = dir
	return b
}

// Env sets an environment variable.
func (b *LocalStackBuilder) Env(key, value string) *LocalStackBuilder {
	b.env[key] = value
	return b
}

// ReadyTimeout sets how long to wait for LocalStack to be ready.
func (b *LocalStackBuilder) ReadyTimeout(d time.Duration) *LocalStackBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *LocalStackBuilder) WithCLI(cli *DockerCLI) *LocalStackBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the LocalStack container.
func (b *LocalStackBuilder) Start(ctx context.Context) (*LocalStackContainer, error) {
	// Default to common services if none specified
	if len(b.services) == 0 {
		b.services = []string{"sqs", "dynamodb"}
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("localstack").
		Port(b.edgePort, 4566).
		Env("SERVICES", strings.Join(b.services, ",")).
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Debug mode
	if b.debug {
		builder.Env("DEBUG", "1")
	}

	// Data persistence
	if b.dataDir != "" {
		builder.Volume(b.dataDir, "/var/lib/localstack")
	}

	// Additional environment variables
	for k, v := range b.env {
		builder.Env(k, v)
	}

	// Add ready check that verifies LocalStack health endpoint
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		port := c.GetHostPort(4566)
		if port == 0 {
			return fmt.Errorf("edge port not mapped")
		}

		// Check health endpoint
		url := fmt.Sprintf("http://localhost:%d/_localstack/health", port)
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

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("health check failed: %d - %s", resp.StatusCode, string(body))
		}

		return nil
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		return nil, err
	}

	return &LocalStackContainer{
		Container: container,
		edgePort:  container.GetHostPort(4566),
		services:  b.services,
	}, nil
}

// ============================================================================
// LocalStack Test Helpers
// ============================================================================

// DefaultLocalStackConfig returns a default configuration with SQS and DynamoDB.
func DefaultLocalStackConfig() *LocalStackBuilder {
	return NewLocalStack().
		WithSQS().
		WithDynamoDB()
}

// LocalStackForSQS returns a configuration optimized for SQS testing.
func LocalStackForSQS() *LocalStackBuilder {
	return NewLocalStack().
		WithSQS()
}

// LocalStackForDynamoDB returns a configuration optimized for DynamoDB testing.
func LocalStackForDynamoDB() *LocalStackBuilder {
	return NewLocalStack().
		WithDynamoDB()
}

// LocalStackFull returns a configuration with common AWS services.
func LocalStackFull() *LocalStackBuilder {
	return NewLocalStack().
		WithSQS().
		WithDynamoDB().
		WithS3().
		WithSNS()
}

