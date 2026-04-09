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
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
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
	} else if !fromEnv && containerName != "" {
		if !isContainerRunning(containerName) {
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
		removeOrphans(containerPrefix)
	}

	amqpPort, err := freePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free AMQP port: %w", err)
	}
	webPort, err := freePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free web port: %w", err)
	}

	name := fmt.Sprintf("%s%d", containerPrefix, amqpPort)
	_ = exec.Command("docker", "rm", "-f", name).Run()

	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}

	out, err := exec.Command("docker", "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:5672", amqpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:8161", webPort),
		"-e", "AMQ_USER="+user(),
		"-e", "AMQ_PASSWORD="+password(),
		"-e", "AMQ_EXTRA_ARGS=--relax-jolokia",
		imageName(),
	).CombinedOutput()
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	containerName = name

	if err := waitForContainerHealthy(name, 30*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("container: %w", err)
	}

	if err := waitForTCP(amqpPort, 60*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("AMQP port: %w", err)
	}

	console := fmt.Sprintf("http://127.0.0.1:%d", webPort)
	if err := waitForConsole(console, 60*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("web console: %w", err)
	}

	if err := stabilize(amqpPort); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("stabilization: %w", err)
	}

	ep := fmt.Sprintf("amqp://127.0.0.1:%d", amqpPort)
	return ep, console, cleanup, nil
}

func waitForConsole(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := baseURL + "/console"
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("console not ready within %v: %v", timeout, lastErr)
}

func removeOrphans(prefix string) {
	out, err := exec.Command("docker", "ps", "-aq",
		"--filter", "name="+prefix).Output()
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		_ = exec.Command("docker", args...).Run()
	}
}

func isContainerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{.State.Running}}", name).Output()
	return err == nil && len(out) > 0 && out[0] == 't'
}

func waitForContainerHealthy(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{.State.Running}} {{.State.ExitCode}}", name).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if strings.HasPrefix(s, "true") {
				return nil
			}
			if strings.Contains(s, "false") {
				return fmt.Errorf("container %s exited (inspect: %s)", name, s)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not reach running state within %v", name, timeout)
}

func waitForTCP(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("TCP connect to %s failed within %v: %v", addr, timeout, lastErr)
}

func stabilize(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 3; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		_ = conn.Close()
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func logContainerFailure(name string) {
	out, _ := exec.Command("docker", "logs", "--tail", "50", name).CombinedOutput()
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "--- docker logs %s ---\n%s\n--- end ---\n", name, out)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
