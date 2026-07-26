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
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const containerPrefix = "gobridge-mqtt-"

// defaultImage is pinned by digest, like rabbitmqlocal. A floating :latest
// means CI can break on a day nobody changed anything.
const defaultImage = "eclipse-mosquitto:2.0.22@sha256:212f89e1eaeb2c322d6441b64396e3346026674db8fa9c27beac293405c32b3c"

type config struct {
	image            string
	persistence      bool
	webSocket        bool
	cleanOrphans     bool
	maxInflightMsgs  int // -1 = not set, 0 = unlimited
	maxQueuedMsgs    int // -1 = not set, 0 = unlimited
	maxQueuedBytes   int // -1 = not set, 0 = unlimited
	messageSizeLimit int // -1 = not set, 0 = unlimited
	extraConfig      string
	memory           string // e.g. "256m", "512m" — passed to --memory
	cpus             string // e.g. "0.5", "1.0" — passed to --cpus
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
	cfg           = config{
		image:            defaultImage,
		maxInflightMsgs:  -1,
		maxQueuedMsgs:    -1,
		maxQueuedBytes:   -1,
		messageSizeLimit: -1,
	}
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

// WithMaxInflightMessages sets the Mosquitto max_inflight_messages config.
// Use 0 for unlimited. Default (-1) omits the setting (Mosquitto default: 20).
func WithMaxInflightMessages(n int) Option {
	return func(c *config) { c.maxInflightMsgs = n }
}

// WithMaxQueuedMessages sets the Mosquitto max_queued_messages config.
// Use 0 for unlimited. Default (-1) omits the setting (Mosquitto default: 1000).
func WithMaxQueuedMessages(n int) Option {
	return func(c *config) { c.maxQueuedMsgs = n }
}

// WithMaxQueuedBytes sets the Mosquitto max_queued_bytes config.
// Use 0 for unlimited. Default (-1) omits the setting.
func WithMaxQueuedBytes(n int) Option {
	return func(c *config) { c.maxQueuedBytes = n }
}

// WithMessageSizeLimit sets the Mosquitto message_size_limit config.
// Use 0 for unlimited. Default (-1) omits the setting.
func WithMessageSizeLimit(n int) Option {
	return func(c *config) { c.messageSizeLimit = n }
}

// WithExtraConfig appends raw lines to the Mosquitto config file.
// Each line should be terminated with a newline.
func WithExtraConfig(lines string) Option {
	return func(c *config) { c.extraConfig = lines }
}

// WithMemory sets the Docker --memory limit for the container (e.g. "256m", "512m").
func WithMemory(limit string) Option {
	return func(c *config) { c.memory = limit }
}

// WithCPUs sets the Docker --cpus limit for the container (e.g. "0.5", "1.0").
func WithCPUs(limit string) Option {
	return func(c *config) { c.cpus = limit }
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
				initErr = waitBrokerReady(port, 10*time.Second)
			}
		} else {
			brokerURL, wsURL, containerName, cleanupFn, initErr = startContainer(cfg)
		}
	} else if !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			t.Logf("mqttlocal: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			brokerURL, wsURL, containerName, cleanupFn, initErr = startContainer(cfg)
		}
	}

	if initErr != nil {
		// A fixture that will not start is a failure wherever it could have
		// started; skipping here is what let a permanently broken emulator
		// report `ok` for its whole package. See dockerexec.MustSucceed.
		if dockerexec.MustSucceed() {
			t.Fatalf("Mosquitto not available: %v", initErr)
		}
		t.Skipf("Mosquitto not available (docker absent): %v", initErr)
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

// ForceStart kills any existing container and starts a fresh Mosquitto
// container. The container is removed when the test ends via t.Cleanup.
// Returns the MQTT broker URL (tcp://127.0.0.1:<port>).
//
// Use this instead of [BrokerURL] when the test needs a guaranteed-fresh
// container (e.g. resilience or restart tests).
func ForceStart(t testing.TB) string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()

	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
	resolved = false

	dockerexec.RemoveOrphans(containerPrefix)

	mqttURL, wsEndpoint, name, cleanup, err := startContainer(cfg)
	if err != nil {
		t.Fatalf("mqttlocal.ForceStart: %v", err)
	}

	brokerURL = mqttURL
	wsURL = wsEndpoint
	containerName = name
	cleanupFn = cleanup
	resolved = true

	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if cleanupFn != nil {
			cleanupFn()
			cleanupFn = nil
		}
		resolved = false
	})

	return mqttURL
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
	if err := waitBrokerReady(port, 30*time.Second); err != nil {
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
		dockerexec.RemoveOrphans(containerPrefix)
	}

	mqttPort, err := dockerexec.FreePort()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("find free MQTT port: %w", err)
	}

	var wsPort int
	if c.webSocket {
		wsPort, err = dockerexec.FreePort()
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

	// os.CreateTemp makes the file 0600, owned by the user running the tests.
	// A bind mount preserves that uid and mode on Linux, so a container running
	// as a non-root user cannot read it. Nothing secret here.
	if err := os.Chmod(confPath, 0o644); err != nil {
		_ = os.Remove(confPath)
		return "", "", "", nil, fmt.Errorf("chmod config readable by container uid: %w", err)
	}

	name := fmt.Sprintf("gobridge-mqtt-%d", mqttPort)

	// Reclaim the name and wait until docker has forgotten it, so the run
	// below cannot collide with a still-terminating container.
	_ = dockerexec.DrainRemove(name, dockerexec.RemoveTimeout)

	if err := dockerexec.EnsureImage(c.image); err != nil {
		_ = os.Remove(confPath)
		return "", "", "", nil, err
	}

	args := []string{
		"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:1883", mqttPort),
		"-v", confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}

	if c.webSocket && wsPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:9001", wsPort))
	}
	if c.memory != "" {
		args = append(args, "--memory", c.memory)
	}
	if c.cpus != "" {
		args = append(args, "--cpus", c.cpus)
	}

	args = append(args, c.image)

	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		_ = os.Remove(confPath)
		return "", "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	cleanup = func() {
		// Drain so Mosquitto flushes persistence on SIGTERM, and so the
		// published port is released before the next fixture claims one.
		_ = dockerexec.DrainRemove(name, dockerexec.RemoveTimeout)
		_ = os.Remove(confPath)
	}

	if err := dockerexec.WaitHealthy(name, 15*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto container failed: %w", err)
	}

	// Gate on protocol truth, not on the port. Accepting TCP does NOT imply
	// Mosquitto is operational: with Docker's userland proxy the host port is
	// bound at container creation, so a dial succeeds while the broker is
	// still loading config or restoring persistence — or has already exited.
	// waitBrokerReady requires a real publish/deliver roundtrip, so returning
	// from startContainer means the broker moves messages.
	if err := waitBrokerReady(mqttPort, 30*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto not ready: %w", err)
	}

	mqttURL = fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort)
	if c.webSocket && wsPort > 0 {
		wsURLOut = fmt.Sprintf("ws://127.0.0.1:%d", wsPort)
	}

	return mqttURL, wsURLOut, name, cleanup, nil
}

// buildConfig is in helpers.go; container lifecycle gates (WaitHealthy,
// WaitTCP, StabilizeTCP, RemoveOrphans, LogFailure, FreePort) come from
// testutil/dockerexec.
