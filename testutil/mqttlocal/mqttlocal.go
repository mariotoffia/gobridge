// Package mqttlocal provides shared test infrastructure for a Mosquitto
// MQTT broker running in Docker.
//
// It manages the lifecycle of a Mosquitto container and provides helpers
// for obtaining the broker URL, creating unique client IDs, and cleanup.
// Multiple test packages in the same binary share a single container
// instance via [BrokerURL].
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    code := m.Run()
//	    mqttlocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    url := mqttlocal.BrokerURL(t)
//	    // ... connect to broker, run tests ...
//	}
//
// The container is started on first call to [BrokerURL].
// If the MQTT_BROKER_URL environment variable is set, no container is
// started and that URL is used directly (after verifying connectivity).
//
// # Configuration
//
// The default Mosquitto configuration allows anonymous connections and
// disables persistence (suitable for fast tests). To customise the
// broker, use [Option] functions:
//
//	mqttlocal.Configure(
//	    mqttlocal.WithPersistence(true),
//	    mqttlocal.WithWebSocket(true),
//	)
//
// Call [Configure] before any call to [BrokerURL]. Once the container
// is started, configuration changes are ignored.
package mqttlocal

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const containerPrefix = "gobridge-mqtt-"

type config struct {
	image        string
	persistence  bool
	webSocket    bool
	cleanOrphans bool
}

var (
	mu            sync.Mutex
	resolved      bool
	fromEnv       bool
	brokerURL     string
	wsURL         string
	containerName string
	cleanupFn     func()
	initErr       error
	cfg           = config{image: "eclipse-mosquitto:latest"}
)

// Option configures the Mosquitto container before it is started.
type Option func(*config)

// WithImage sets the Docker image for Mosquitto.
func WithImage(image string) Option {
	return func(c *config) { c.image = image }
}

// WithPersistence enables Mosquitto persistence. By default persistence
// is disabled for fast, isolated tests.
func WithPersistence(enabled bool) Option {
	return func(c *config) { c.persistence = enabled }
}

// WithWebSocket enables the WebSocket listener on port 9001.
func WithWebSocket(enabled bool) Option {
	return func(c *config) { c.webSocket = enabled }
}

// WithCleanOrphans enables removal of all leftover gobridge-mqtt-*
// containers before starting a new one. Recommended for CI and long
// test suites to prevent resource leaks from crashed runs.
func WithCleanOrphans(enabled bool) Option {
	return func(c *config) { c.cleanOrphans = enabled }
}

// Configure applies options before the container is started.
// Must be called before the first [BrokerURL] or [WebSocketURL] call.
// Calling after the container is running has no effect.
func Configure(opts ...Option) {
	mu.Lock()
	defer mu.Unlock()
	if resolved {
		return
	}
	for _, o := range opts {
		o(&cfg)
	}
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// BrokerURL returns the Mosquitto MQTT broker URL (tcp://127.0.0.1:<port>).
//
// On first call it checks MQTT_BROKER_URL; if unset it starts a Mosquitto
// Docker container. The test is skipped when -short is set or Docker is
// unavailable.
//
// When a URL is provided via the environment variable, a TCP connectivity
// check is performed before returning it.
//
// Call [Shutdown] in TestMain after m.Run() to stop the container.
func BrokerURL(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping MQTT integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("MQTT_BROKER_URL"); ep != "" {
			brokerURL = ep
			fromEnv = true
			wsURL = os.Getenv("MQTT_WS_URL")
			if port, err := portFromURL(ep); err == nil {
				initErr = waitForTCP(port, 10*time.Second)
			}
		} else {
			brokerURL, wsURL, containerName, cleanupFn, initErr = startContainer(cfg)
		}
	} else if !fromEnv && containerName != "" {
		if !isContainerRunning(containerName) {
			t.Logf("mqttlocal: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			brokerURL, wsURL, containerName, cleanupFn, initErr = startContainer(cfg)
		}
	}

	if initErr != nil {
		t.Skipf("Mosquitto not available: %v", initErr)
	}
	return brokerURL
}

// WebSocketURL returns the Mosquitto WebSocket URL. Returns "" if
// WebSocket is not enabled. Triggers container startup if needed.
func WebSocketURL(t testing.TB) string {
	t.Helper()
	_ = BrokerURL(t)
	return wsURL
}

// Shutdown stops the Mosquitto container if one was started.
// Safe to call multiple times or when no container was started.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueClientID returns a client ID built from prefix and a nanosecond
// timestamp, suitable for test isolation.
func UniqueClientID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// WaitUntilReady verifies that the MQTT broker is accepting TCP
// connections. This is called automatically when starting a container
// or when using an env-provided URL. Can also be called explicitly
// for additional verification.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	u := BrokerURL(t)
	port, err := portFromURL(u)
	if err != nil {
		t.Fatalf("mqttlocal: %v", err)
	}
	if err := waitForTCP(port, 30*time.Second); err != nil {
		t.Fatalf("mqttlocal: %v", err)
	}
}

// portFromURL extracts the port number from a broker URL like
// "tcp://127.0.0.1:1883" or "tcp://localhost:1883".
func portFromURL(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, fmt.Errorf("cannot parse broker URL %q: %w", rawURL, err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, fmt.Errorf("cannot extract port from broker URL %q: %w", rawURL, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port == 0 {
		return 0, fmt.Errorf("invalid port in broker URL %q", rawURL)
	}
	return port, nil
}

// ---------------------------------------------------------------------------
// Container lifecycle
// ---------------------------------------------------------------------------

func startContainer(c config) (mqttURL, wsURLOut, cName string, cleanup func(), err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", "", "", nil, fmt.Errorf("docker not found: %w", err)
	}

	if c.cleanOrphans {
		removeOrphans(containerPrefix)
	}

	mqttPort, err := freePort()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("find free MQTT port: %w", err)
	}

	var wsPort int
	if c.webSocket {
		wsPort, err = freePort()
		if err != nil {
			return "", "", "", nil, fmt.Errorf("find free WebSocket port: %w", err)
		}
	}

	confContent := buildConfig(c, wsPort > 0)

	confFile, err := os.CreateTemp("", "mqttlocal-*.conf")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("create temp config: %w", err)
	}
	if _, err := confFile.WriteString(confContent); err != nil {
		_ = confFile.Close()
		_ = os.Remove(confFile.Name())
		return "", "", "", nil, fmt.Errorf("write config: %w", err)
	}
	_ = confFile.Close()
	confPath := confFile.Name()

	name := fmt.Sprintf("gobridge-mqtt-%d", mqttPort)

	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := []string{
		"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:1883", mqttPort),
		"-v", confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}

	if c.webSocket && wsPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:9001", wsPort))
	}

	args = append(args, c.image)

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(confPath)
		return "", "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	cleanup = func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = os.Remove(confPath)
	}

	if err := waitForContainerHealthy(name, 15*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto container failed: %w", err)
	}

	if err := waitForTCP(mqttPort, 30*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto not ready: %w", err)
	}

	if err := stabilize(mqttPort); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto stabilization failed: %w", err)
	}

	mqttURL = fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort)
	if c.webSocket && wsPort > 0 {
		wsURLOut = fmt.Sprintf("ws://127.0.0.1:%d", wsPort)
	}

	return mqttURL, wsURLOut, name, cleanup, nil
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

func buildConfig(c config, hasWS bool) string {
	s := "listener 1883 0.0.0.0\nprotocol mqtt\n\n"
	if hasWS {
		s += "listener 9001 0.0.0.0\nprotocol websockets\n\n"
	}
	s += "allow_anonymous true\n\n"
	if c.persistence {
		s += "persistence true\npersistence_location /mosquitto/data/\n"
	} else {
		s += "persistence false\n"
	}
	s += "\nlog_dest stdout\n"
	return s
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
		time.Sleep(200 * time.Millisecond)
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
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func logContainerFailure(name string) {
	out, _ := exec.Command("docker", "logs", "--tail", "30", name).CombinedOutput()
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
