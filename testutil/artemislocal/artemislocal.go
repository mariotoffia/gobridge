// Package artemislocal provides shared test infrastructure for an
// Apache ActiveMQ Artemis broker running in Docker.
//
// It manages a single Artemis container and provides helpers for
// creating AMQP 1.0 clients and addresses. Multiple test packages
// in the same binary share a single container via [Endpoint].
//
// The container is automatically restarted if it dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    artemislocal.Configure(artemislocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    artemislocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    ep := artemislocal.Endpoint(t) // "amqp://127.0.0.1:<port>"
//	    // ... create sender/receiver with endpoint ...
//	}
//
// The container is started on first call to [Endpoint].
// If the ARTEMIS_URL environment variable is set, no container is
// started and that URL is used directly.
package artemislocal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const (
	containerPrefix = "gobridge-artemis-"
	defaultImage    = "apache/activemq-artemis:latest-alpine"
	defaultUser     = "admin"
	defaultPassword = "admin"
)

type options struct {
	cleanOrphans bool
	image        string
	user         string
	password     string
}

var (
	mu            sync.Mutex
	resolved      bool
	fromEnv       bool
	endpoint      string
	consoleURL    string
	containerName string
	cleanupFn     func()
	initErr       error
	opts          options
)

// Option configures the Artemis test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-artemis-*
// containers before starting new ones.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithImage overrides the default Docker image.
func WithImage(image string) Option {
	return func(o *options) { o.image = image }
}

// WithCredentials overrides the default admin/admin credentials.
func WithCredentials(user, password string) Option {
	return func(o *options) { o.user = user; o.password = password }
}

// Configure applies options before the container is started.
// Must be called before the first [Endpoint] call.
func Configure(fns ...Option) {
	mu.Lock()
	defer mu.Unlock()
	if resolved {
		return
	}
	for _, fn := range fns {
		fn(&opts)
	}
}

// Endpoint returns the AMQP 1.0 broker URL.
//
// On first call it checks ARTEMIS_URL; if unset it starts an Artemis
// container in Docker. The test is skipped when -short is set or Docker
// is unavailable.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Artemis integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if url := os.Getenv("ARTEMIS_URL"); url != "" {
			endpoint = url
			fromEnv = true
		} else {
			endpoint, consoleURL, cleanupFn, initErr = startContainer()
		}
	} else if initErr == nil && !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, consoleURL, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		t.Skipf("Artemis not available: %v", initErr)
	}
	return endpoint
}

// ConsoleURL returns the Artemis web console URL.
func ConsoleURL(t testing.TB) string {
	t.Helper()
	_ = Endpoint(t)
	mu.Lock()
	defer mu.Unlock()
	return consoleURL
}

// Credentials returns the configured username and password.
func Credentials() (string, string) {
	return user(), password()
}

// Shutdown stops the Artemis container. Safe to call multiple times.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueAddress returns an address name with a nanosecond timestamp suffix.
func UniqueAddress(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ForceStart resets global state and starts a fresh container.
// Registers t.Cleanup to tear down.
func ForceStart(t testing.TB) string {
	t.Helper()
	mu.Lock()
	if cleanupFn != nil {
		cleanupFn()
	}
	resolved = false
	fromEnv = false
	endpoint = ""
	consoleURL = ""
	containerName = ""
	cleanupFn = nil
	initErr = nil
	mu.Unlock()

	ep := Endpoint(t)
	t.Cleanup(func() {
		mu.Lock()
		if cleanupFn != nil {
			cleanupFn()
			cleanupFn = nil
		}
		resolved = false
		mu.Unlock()
	})
	return ep
}

func user() string {
	if opts.user != "" {
		return opts.user
	}
	return defaultUser
}

func password() string {
	if opts.password != "" {
		return opts.password
	}
	return defaultPassword
}

func imageName() string {
	if opts.image != "" {
		return opts.image
	}
	return defaultImage
}

func startContainer() (string, string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", "", nil, fmt.Errorf("docker not found: %w", err)
	}

	if opts.cleanOrphans {
		dockerexec.RemoveOrphans(containerPrefix)
	}

	amqpPort, err := dockerexec.FreePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free AMQP port: %w", err)
	}
	webPort, err := dockerexec.FreePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free web port: %w", err)
	}

	name := fmt.Sprintf("%s%d", containerPrefix, amqpPort)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)

	cleanup := func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	}

	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:5672", amqpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:8161", webPort),
		"-e", "ARTEMIS_USER="+user(),
		"-e", "ARTEMIS_PASSWORD="+password(),
		"-e", "EXTRA_ARGS=--relax-jolokia",
		imageName(),
	)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	if err := dockerexec.WaitHealthy(name, 30*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("container: %w", err)
	}

	if err := dockerexec.WaitTCP(amqpPort, 60*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("AMQP port: %w", err)
	}

	if err := dockerexec.StabilizeTCP(amqpPort); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("stabilization: %w", err)
	}

	// Protocol truth: Artemis accepts TCP well before its AMQP acceptor
	// authenticates — gate on a real SASL dial, not the socket.
	amqpEP := fmt.Sprintf("amqp://127.0.0.1:%d", amqpPort)
	if err := dockerexec.WaitProbe("Artemis AMQP on "+amqpEP, 30*time.Second, time.Second,
		amqpProbe(amqpEP)); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, err
	}

	containerName = name
	console := fmt.Sprintf("http://127.0.0.1:%d", webPort)
	ep := fmt.Sprintf("amqp://127.0.0.1:%d", amqpPort)
	return ep, console, cleanup, nil
}

// amqpProbe gates on a real AMQP 1.0 SASL dial — success proves the broker
// authenticates and speaks the protocol, not merely that the port is open.
func amqpProbe(ep string) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		conn, err := amqp.Dial(ctx, ep, &amqp.ConnOptions{
			SASLType: amqp.SASLTypePlain(user(), password()),
		})
		if err != nil {
			return err
		}
		return conn.Close()
	}
}
