// Package docker provides test utilities for managing Docker containers
// in integration tests. It uses the Docker CLI via os/exec for simplicity
// and no external dependencies.
//
// # Usage
//
// Use the ContainerBuilder to configure and start containers:
//
//	container, err := docker.NewContainerBuilder().
//	    Image("nginx:latest").
//	    Name("test-nginx").
//	    Port(8080, 80).
//	    Env("NGINX_HOST", "localhost").
//	    Start(ctx)
//
// All test containers are automatically labeled for orphan cleanup.
// Call CleanupOrphans() in TestMain to remove leftover containers.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Session Management
// ============================================================================

var (
	// sessionID uniquely identifies this test session for orphan tracking.
	sessionID     string
	sessionIDOnce sync.Once
)

// getSessionID returns a unique session ID for this test run.
// Used for labeling containers and identifying orphans.
func getSessionID() string {
	sessionIDOnce.Do(func() {
		sessionID = fmt.Sprintf("%d", time.Now().UnixNano())
	})
	return sessionID
}

// Labels applied to all test containers.
const (
	LabelTest    = "gobridge.test"
	LabelService = "gobridge.service"
	LabelSession = "gobridge.session"
)

// ============================================================================
// DockerCLI
// ============================================================================

// DockerCLI wraps the docker command-line interface.
type DockerCLI struct {
	// dockerPath is the path to the docker binary.
	dockerPath string
}

// NewDockerCLI creates a new DockerCLI instance.
func NewDockerCLI() *DockerCLI {
	return &DockerCLI{
		dockerPath: "docker",
	}
}

// WithDockerPath sets a custom path to the docker binary.
func (c *DockerCLI) WithDockerPath(path string) *DockerCLI {
	c.dockerPath = path
	return c
}

// Run executes a docker command and returns the output.
func (c *DockerCLI) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.dockerPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// IsAvailable checks if Docker is available and running.
func (c *DockerCLI) IsAvailable(ctx context.Context) bool {
	_, err := c.Run(ctx, "info")
	return err == nil
}

// ============================================================================
// Container
// ============================================================================

// Container represents a running Docker container.
type Container struct {
	// ID is the container ID.
	ID string

	// Name is the container name.
	Name string

	// Image is the image used to create the container.
	Image string

	// Ports maps host ports to container ports.
	Ports map[int]int

	// cli is the DockerCLI used to manage this container.
	cli *DockerCLI
}

// Stop stops the container without removing it.
func (c *Container) Stop(ctx context.Context) error {
	_, err := c.cli.Run(ctx, "stop", c.ID)
	return err
}

// Remove stops and removes the container.
func (c *Container) Remove(ctx context.Context) error {
	_, err := c.cli.Run(ctx, "rm", "-f", c.ID)
	return err
}

// Logs returns the container logs.
func (c *Container) Logs(ctx context.Context) (string, error) {
	return c.cli.Run(ctx, "logs", c.ID)
}

// IsRunning checks if the container is running.
func (c *Container) IsRunning(ctx context.Context) bool {
	output, err := c.cli.Run(ctx, "inspect", "-f", "{{.State.Running}}", c.ID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "true"
}

// Exec runs a command inside the container.
func (c *Container) Exec(ctx context.Context, cmd ...string) (string, error) {
	args := append([]string{"exec", c.ID}, cmd...)
	return c.cli.Run(ctx, args...)
}

// GetHostPort returns the host port mapped to the given container port.
// Returns 0 if the port is not mapped.
func (c *Container) GetHostPort(containerPort int) int {
	for host, container := range c.Ports {
		if container == containerPort {
			return host
		}
	}
	return 0
}

// ============================================================================
// ContainerOptions
// ============================================================================

// ContainerOptions holds configuration for creating a container.
type ContainerOptions struct {
	// Image is the Docker image to use.
	Image string

	// Name is the container name (optional).
	Name string

	// Labels are applied to the container for filtering.
	Labels map[string]string

	// Ports maps host ports to container ports.
	// Use 0 for host port to get a random available port.
	Ports map[int]int

	// Env holds environment variables.
	Env map[string]string

	// Volumes maps host paths to container paths.
	Volumes map[string]string

	// Cmd is the command to run (overrides image default).
	Cmd []string

	// Entrypoint overrides the image entrypoint.
	Entrypoint []string

	// Network is the network to connect to.
	Network string

	// HealthCheck is a command to check container health.
	HealthCheck string

	// HealthCheckInterval is how often to run the health check.
	HealthCheckInterval time.Duration

	// HealthCheckRetries is how many times to retry health check.
	HealthCheckRetries int

	// ReadyTimeout is how long to wait for the container to be ready.
	ReadyTimeout time.Duration

	// ReadyCheck is a function to check if the container is ready.
	// If nil, waits for the container to be running.
	ReadyCheck func(ctx context.Context, c *Container) error

	// ServiceType is the service type for labeling (e.g., "mosquitto", "localstack").
	ServiceType string

	// TmpFs maps container paths to tmpfs options.
	TmpFs map[string]string

	// CapAdd adds Linux capabilities.
	CapAdd []string

	// User sets the user to run as.
	User string

	// WorkDir sets the working directory.
	WorkDir string

	// Hostname sets the container hostname.
	Hostname string

	// ExtraHosts adds extra /etc/hosts entries.
	ExtraHosts []string

	// Pull forces image pull before starting.
	Pull bool
}

// ============================================================================
// ContainerBuilder
// ============================================================================

// ContainerBuilder provides a fluent API for configuring containers.
type ContainerBuilder struct {
	opts ContainerOptions
	cli  *DockerCLI
}

// NewContainerBuilder creates a new ContainerBuilder.
func NewContainerBuilder() *ContainerBuilder {
	return &ContainerBuilder{
		opts: ContainerOptions{
			Labels:              make(map[string]string),
			Ports:               make(map[int]int),
			Env:                 make(map[string]string),
			Volumes:             make(map[string]string),
			TmpFs:               make(map[string]string),
			HealthCheckInterval: 1 * time.Second,
			HealthCheckRetries:  30,
			ReadyTimeout:        30 * time.Second,
		},
		cli: NewDockerCLI(),
	}
}

// Image sets the Docker image.
func (b *ContainerBuilder) Image(image string) *ContainerBuilder {
	b.opts.Image = image
	return b
}

// Name sets the container name.
func (b *ContainerBuilder) Name(name string) *ContainerBuilder {
	b.opts.Name = name
	return b
}

// Label adds a label to the container.
func (b *ContainerBuilder) Label(key, value string) *ContainerBuilder {
	b.opts.Labels[key] = value
	return b
}

// Port maps a host port to a container port.
// Use hostPort=0 for a random available port.
func (b *ContainerBuilder) Port(hostPort, containerPort int) *ContainerBuilder {
	b.opts.Ports[hostPort] = containerPort
	return b
}

// Env sets an environment variable.
func (b *ContainerBuilder) Env(key, value string) *ContainerBuilder {
	b.opts.Env[key] = value
	return b
}

// Volume mounts a host path to a container path.
func (b *ContainerBuilder) Volume(hostPath, containerPath string) *ContainerBuilder {
	b.opts.Volumes[hostPath] = containerPath
	return b
}

// Cmd sets the command to run.
func (b *ContainerBuilder) Cmd(cmd ...string) *ContainerBuilder {
	b.opts.Cmd = cmd
	return b
}

// Entrypoint sets the entrypoint.
func (b *ContainerBuilder) Entrypoint(entrypoint ...string) *ContainerBuilder {
	b.opts.Entrypoint = entrypoint
	return b
}

// Network sets the network to connect to.
func (b *ContainerBuilder) Network(network string) *ContainerBuilder {
	b.opts.Network = network
	return b
}

// HealthCheck sets a health check command.
func (b *ContainerBuilder) HealthCheck(cmd string) *ContainerBuilder {
	b.opts.HealthCheck = cmd
	return b
}

// HealthCheckInterval sets the health check interval.
func (b *ContainerBuilder) HealthCheckInterval(d time.Duration) *ContainerBuilder {
	b.opts.HealthCheckInterval = d
	return b
}

// HealthCheckRetries sets the number of health check retries.
func (b *ContainerBuilder) HealthCheckRetries(n int) *ContainerBuilder {
	b.opts.HealthCheckRetries = n
	return b
}

// ReadyTimeout sets how long to wait for the container to be ready.
func (b *ContainerBuilder) ReadyTimeout(d time.Duration) *ContainerBuilder {
	b.opts.ReadyTimeout = d
	return b
}

// ReadyCheck sets a custom ready check function.
func (b *ContainerBuilder) ReadyCheck(fn func(ctx context.Context, c *Container) error) *ContainerBuilder {
	b.opts.ReadyCheck = fn
	return b
}

// ServiceType sets the service type for labeling.
func (b *ContainerBuilder) ServiceType(serviceType string) *ContainerBuilder {
	b.opts.ServiceType = serviceType
	return b
}

// TmpFs mounts a tmpfs at the given path.
func (b *ContainerBuilder) TmpFs(containerPath, options string) *ContainerBuilder {
	b.opts.TmpFs[containerPath] = options
	return b
}

// CapAdd adds a Linux capability.
func (b *ContainerBuilder) CapAdd(cap string) *ContainerBuilder {
	b.opts.CapAdd = append(b.opts.CapAdd, cap)
	return b
}

// User sets the user to run as.
func (b *ContainerBuilder) User(user string) *ContainerBuilder {
	b.opts.User = user
	return b
}

// WorkDir sets the working directory.
func (b *ContainerBuilder) WorkDir(dir string) *ContainerBuilder {
	b.opts.WorkDir = dir
	return b
}

// Hostname sets the container hostname.
func (b *ContainerBuilder) Hostname(hostname string) *ContainerBuilder {
	b.opts.Hostname = hostname
	return b
}

// ExtraHost adds an /etc/hosts entry.
func (b *ContainerBuilder) ExtraHost(host string) *ContainerBuilder {
	b.opts.ExtraHosts = append(b.opts.ExtraHosts, host)
	return b
}

// Pull forces image pull before starting.
func (b *ContainerBuilder) Pull() *ContainerBuilder {
	b.opts.Pull = true
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *ContainerBuilder) WithCLI(cli *DockerCLI) *ContainerBuilder {
	b.cli = cli
	return b
}

// Build returns the ContainerOptions without starting the container.
func (b *ContainerBuilder) Build() ContainerOptions {
	return b.opts
}

// Start creates and starts the container.
func (b *ContainerBuilder) Start(ctx context.Context) (*Container, error) {
	if b.opts.Image == "" {
		return nil, fmt.Errorf("image is required")
	}

	// Pull image if requested
	if b.opts.Pull {
		if _, err := b.cli.Run(ctx, "pull", b.opts.Image); err != nil {
			return nil, fmt.Errorf("failed to pull image: %w", err)
		}
	}

	// Build docker run command
	args := []string{"run", "-d"}

	// Add standard test labels
	args = append(args, "--label", fmt.Sprintf("%s=true", LabelTest))
	args = append(args, "--label", fmt.Sprintf("%s=%s", LabelSession, getSessionID()))
	if b.opts.ServiceType != "" {
		args = append(args, "--label", fmt.Sprintf("%s=%s", LabelService, b.opts.ServiceType))
	}

	// Add custom labels
	for k, v := range b.opts.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// Add name
	if b.opts.Name != "" {
		args = append(args, "--name", b.opts.Name)
	}

	// Add ports
	for hostPort, containerPort := range b.opts.Ports {
		if hostPort == 0 {
			args = append(args, "-p", fmt.Sprintf("%d", containerPort))
		} else {
			args = append(args, "-p", fmt.Sprintf("%d:%d", hostPort, containerPort))
		}
	}

	// Add environment variables
	for k, v := range b.opts.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Add volumes
	for hostPath, containerPath := range b.opts.Volumes {
		args = append(args, "-v", fmt.Sprintf("%s:%s", hostPath, containerPath))
	}

	// Add tmpfs
	for containerPath, options := range b.opts.TmpFs {
		if options == "" {
			args = append(args, "--tmpfs", containerPath)
		} else {
			args = append(args, "--tmpfs", fmt.Sprintf("%s:%s", containerPath, options))
		}
	}

	// Add network
	if b.opts.Network != "" {
		args = append(args, "--network", b.opts.Network)
	}

	// Add health check
	if b.opts.HealthCheck != "" {
		args = append(args, "--health-cmd", b.opts.HealthCheck)
		args = append(args, "--health-interval", b.opts.HealthCheckInterval.String())
		args = append(args, "--health-retries", strconv.Itoa(b.opts.HealthCheckRetries))
	}

	// Add capabilities
	for _, cap := range b.opts.CapAdd {
		args = append(args, "--cap-add", cap)
	}

	// Add user
	if b.opts.User != "" {
		args = append(args, "--user", b.opts.User)
	}

	// Add workdir
	if b.opts.WorkDir != "" {
		args = append(args, "--workdir", b.opts.WorkDir)
	}

	// Add hostname
	if b.opts.Hostname != "" {
		args = append(args, "--hostname", b.opts.Hostname)
	}

	// Add extra hosts
	for _, host := range b.opts.ExtraHosts {
		args = append(args, "--add-host", host)
	}

	// Add entrypoint
	if len(b.opts.Entrypoint) > 0 {
		args = append(args, "--entrypoint", b.opts.Entrypoint[0])
	}

	// Add image
	args = append(args, b.opts.Image)

	// Add entrypoint args (if more than one)
	if len(b.opts.Entrypoint) > 1 {
		args = append(args, b.opts.Entrypoint[1:]...)
	}

	// Add command
	args = append(args, b.opts.Cmd...)

	// Run container
	containerID, err := b.cli.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Create container struct
	container := &Container{
		ID:    containerID,
		Name:  b.opts.Name,
		Image: b.opts.Image,
		Ports: make(map[int]int),
		cli:   b.cli,
	}

	// Resolve port mappings
	for hostPort, containerPort := range b.opts.Ports {
		if hostPort == 0 {
			// Get the dynamically assigned port
			resolvedPort, err := b.resolvePort(ctx, containerID, containerPort)
			if err != nil {
				_ = container.Remove(ctx)
				return nil, fmt.Errorf("failed to resolve port %d: %w", containerPort, err)
			}
			container.Ports[resolvedPort] = containerPort
		} else {
			container.Ports[hostPort] = containerPort
		}
	}

	// Wait for container to be ready
	if err := b.waitForReady(ctx, container); err != nil {
		_ = container.Remove(ctx)
		return nil, fmt.Errorf("container not ready: %w", err)
	}

	return container, nil
}

// resolvePort gets the host port for a dynamically assigned container port.
func (b *ContainerBuilder) resolvePort(ctx context.Context, containerID string, containerPort int) (int, error) {
	output, err := b.cli.Run(ctx, "port", containerID, strconv.Itoa(containerPort))
	if err != nil {
		return 0, err
	}

	// Output format: "0.0.0.0:32768" or "[::]:32768"
	parts := strings.Split(output, ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("unexpected port output: %s", output)
	}

	// Get the last part (port number), handling multiple lines
	lines := strings.Split(parts[len(parts)-1], "\n")
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, fmt.Errorf("failed to parse port: %w", err)
	}

	return port, nil
}

// waitForReady waits for the container to be ready.
func (b *ContainerBuilder) waitForReady(ctx context.Context, container *Container) error {
	ctx, cancel := context.WithTimeout(ctx, b.opts.ReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logs, _ := container.Logs(context.Background())
			return fmt.Errorf("timeout waiting for container to be ready: %w\nlogs:\n%s", ctx.Err(), logs)
		case <-ticker.C:
			// Check if container is still running (use background context for inspect)
			if !container.IsRunning(context.Background()) {
				logs, _ := container.Logs(context.Background())
				return fmt.Errorf("container exited unexpectedly\nlogs:\n%s", logs)
			}

			// Run custom ready check if provided
			if b.opts.ReadyCheck != nil {
				if err := b.opts.ReadyCheck(ctx, container); err == nil {
					return nil
				}
				continue
			}

			// Default: check health status if health check is configured
			if b.opts.HealthCheck != "" {
				output, err := b.cli.Run(ctx, "inspect", "-f", "{{.State.Health.Status}}", container.ID)
				if err != nil {
					continue
				}
				if strings.TrimSpace(output) == "healthy" {
					return nil
				}
				continue
			}

			// No health check - just check if running
			return nil
		}
	}
}

// ============================================================================
// Orphan Cleanup
// ============================================================================

// CleanupOrphans removes containers from previous test sessions.
// This should be called in TestMain to clean up any leftover containers.
func CleanupOrphans(ctx context.Context) error {
	cli := NewDockerCLI()
	return cleanupWithFilter(ctx, cli, fmt.Sprintf("label=%s=true", LabelTest))
}

// CleanupSession removes all containers from the current test session.
// This should be called at the end of TestMain.
func CleanupSession(ctx context.Context) error {
	cli := NewDockerCLI()
	return cleanupWithFilter(ctx, cli,
		fmt.Sprintf("label=%s=true", LabelTest),
		fmt.Sprintf("label=%s=%s", LabelSession, getSessionID()))
}

// CleanupService removes all containers for a specific service type.
func CleanupService(ctx context.Context, serviceType string) error {
	cli := NewDockerCLI()
	return cleanupWithFilter(ctx, cli,
		fmt.Sprintf("label=%s=true", LabelTest),
		fmt.Sprintf("label=%s=%s", LabelService, serviceType))
}

// cleanupWithFilter removes containers matching the given filters.
func cleanupWithFilter(ctx context.Context, cli *DockerCLI, filters ...string) error {
	args := []string{"ps", "-aq"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}

	output, err := cli.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	if output == "" {
		return nil
	}

	containerIDs := strings.Fields(output)
	if len(containerIDs) == 0 {
		return nil
	}

	args = append([]string{"rm", "-f"}, containerIDs...)
	_, err = cli.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("failed to remove containers: %w", err)
	}

	return nil
}

// ============================================================================
// Test Helpers
// ============================================================================

// RequireDocker skips the test if Docker is not available.
func RequireDocker(t interface{ Skip(...any) }) {
	cli := NewDockerCLI()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !cli.IsAvailable(ctx) {
		t.Skip("Docker is not available")
	}
}

// WithDockerCleanup runs tests with automatic cleanup.
// Use in TestMain:
//
//	func TestMain(m *testing.M) {
//	    os.Exit(docker.WithDockerCleanup(m))
//	}
func WithDockerCleanup(m interface{ Run() int }) int {
	ctx := context.Background()

	// Clean up orphans before running tests
	_ = CleanupOrphans(ctx)

	// Run tests
	code := m.Run()

	// Clean up session containers
	_ = CleanupSession(ctx)

	return code
}

// CreateTempDir creates a temporary directory that will be cleaned up automatically.
// The directory is created inside the system temp directory.
func CreateTempDir(prefix string) (string, func(), error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		os.RemoveAll(dir)
	}
	return dir, cleanup, nil
}

// WriteFile writes content to a file, creating parent directories if needed.
func WriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
