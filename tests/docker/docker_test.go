// ═══════════════════════════════════════════════════════════════════════════
// Docker Test Utilities - Core Unit Tests
//
// Tests for ContainerBuilder, Container, and core functionality.
// These tests do NOT require Docker to be running.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ U001 │ ContainerBuilder accumulates options   │ PASS     │
// │ U002 │ ContainerBuilder has sensible defaults │ PASS     │
// │ U003 │ Container port lookup                  │ PASS     │
// │ U004 │ Session ID is stable within process    │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// ContainerBuilder Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContainerBuilder_Build validates that the builder correctly accumulates
// all configuration options into ContainerOptions.
//
// Flow:
//   Builder → Image() → Name() → Port() → Env() → Build() → Options
func TestContainerBuilder_Build(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*ContainerBuilder) *ContainerBuilder
		validate func(*testing.T, ContainerOptions)
	}{
		{
			name: "image and name",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("nginx:latest").Name("test-nginx")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "nginx:latest", opts.Image)
				assert.Equal(t, "test-nginx", opts.Name)
			},
		},
		{
			name: "port mappings",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("nginx").Port(8080, 80).Port(8443, 443)
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, 80, opts.Ports[8080])
				assert.Equal(t, 443, opts.Ports[8443])
			},
		},
		{
			name: "environment variables",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Env("DEBUG", "true").Env("PORT", "8080")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "true", opts.Env["DEBUG"])
				assert.Equal(t, "8080", opts.Env["PORT"])
			},
		},
		{
			name: "volumes",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Volume("/host/path", "/container/path")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "/container/path", opts.Volumes["/host/path"])
			},
		},
		{
			name: "labels",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Label("env", "test").Label("version", "1.0")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "test", opts.Labels["env"])
				assert.Equal(t, "1.0", opts.Labels["version"])
			},
		},
		{
			name: "network",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Network("my-network")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "my-network", opts.Network)
			},
		},
		{
			name: "command and entrypoint",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Cmd("run", "--verbose").Entrypoint("/bin/sh", "-c")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, []string{"run", "--verbose"}, opts.Cmd)
				assert.Equal(t, []string{"/bin/sh", "-c"}, opts.Entrypoint)
			},
		},
		{
			name: "health check",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").HealthCheck("curl -f http://localhost/health")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "curl -f http://localhost/health", opts.HealthCheck)
			},
		},
		{
			name: "service type",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").ServiceType("mosquitto")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "mosquitto", opts.ServiceType)
			},
		},
		{
			name: "capabilities",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").CapAdd("NET_ADMIN").CapAdd("SYS_TIME")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Contains(t, opts.CapAdd, "NET_ADMIN")
				assert.Contains(t, opts.CapAdd, "SYS_TIME")
			},
		},
		{
			name: "user and workdir",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").User("1000:1000").WorkDir("/app")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "1000:1000", opts.User)
				assert.Equal(t, "/app", opts.WorkDir)
			},
		},
		{
			name: "hostname and extra hosts",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Hostname("myhost").ExtraHost("other:192.168.1.1")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "myhost", opts.Hostname)
				assert.Contains(t, opts.ExtraHosts, "other:192.168.1.1")
			},
		},
		{
			name: "tmpfs",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").TmpFs("/tmp", "size=100m")
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.Equal(t, "size=100m", opts.TmpFs["/tmp"])
			},
		},
		{
			name: "pull flag",
			setup: func(b *ContainerBuilder) *ContainerBuilder {
				return b.Image("app").Pull()
			},
			validate: func(t *testing.T, opts ContainerOptions) {
				assert.True(t, opts.Pull)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := NewContainerBuilder()
			builder = tc.setup(builder)
			opts := builder.Build()
			tc.validate(t, opts)
		})
	}
}

// TestContainerBuilder_Defaults validates that default values are sensible.
func TestContainerBuilder_Defaults(t *testing.T) {
	builder := NewContainerBuilder()
	opts := builder.Build()

	// Verify defaults
	assert.NotNil(t, opts.Labels, "Labels map should be initialized")
	assert.NotNil(t, opts.Ports, "Ports map should be initialized")
	assert.NotNil(t, opts.Env, "Env map should be initialized")
	assert.NotNil(t, opts.Volumes, "Volumes map should be initialized")
	assert.NotNil(t, opts.TmpFs, "TmpFs map should be initialized")

	// Verify timeouts have sensible defaults
	assert.Greater(t, opts.ReadyTimeout.Seconds(), float64(0), "ReadyTimeout should be positive")
	assert.Greater(t, opts.HealthCheckInterval.Seconds(), float64(0), "HealthCheckInterval should be positive")
	assert.Greater(t, opts.HealthCheckRetries, 0, "HealthCheckRetries should be positive")
}

// ═══════════════════════════════════════════════════════════════════════════
// Container Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContainer_GetHostPort validates port lookup logic.
func TestContainer_GetHostPort(t *testing.T) {
	container := &Container{
		Ports: map[int]int{
			8080: 80,
			8443: 443,
			1883: 1883,
		},
	}

	tests := []struct {
		name          string
		containerPort int
		expectedHost  int
	}{
		{"HTTP port", 80, 8080},
		{"HTTPS port", 443, 8443},
		{"MQTT port", 1883, 1883},
		{"unmapped port", 9999, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := container.GetHostPort(tc.containerPort)
			assert.Equal(t, tc.expectedHost, host)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Session ID Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSessionID_Consistency validates session ID is stable within a process.
func TestSessionID_Consistency(t *testing.T) {
	id1 := getSessionID()
	id2 := getSessionID()
	id3 := getSessionID()

	require.NotEmpty(t, id1, "session ID should not be empty")
	assert.Equal(t, id1, id2, "session ID should be consistent")
	assert.Equal(t, id2, id3, "session ID should be consistent")
}
