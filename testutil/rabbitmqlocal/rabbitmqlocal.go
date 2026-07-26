// Package rabbitmqlocal provides shared test infrastructure for a
// RabbitMQ broker running in Docker.
//
// It manages a single RabbitMQ container with the management plugin
// enabled and provides helpers for creating clients, queues, and
// exchanges. Multiple test packages in the same binary share a single
// container via [Endpoint].
//
// The container is automatically restarted if it dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    rabbitmqlocal.Configure(rabbitmqlocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    rabbitmqlocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    ep := rabbitmqlocal.Endpoint(t) // "amqp://guest:guest@127.0.0.1:<port>/"
//	    // ... create sender/receiver with endpoint ...
//	}
//
// The container is started on first call to [Endpoint].
// If the RABBITMQ_URL environment variable is set, no container is
// started and that URL is used directly.
package rabbitmqlocal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const (
	containerPrefix = "gobridge-rabbitmq-"
	defaultImage    = "rabbitmq:4.2.5-management-alpine@sha256:43553fb6af12bfcf0ed95fbb1c4c658d2b96eed021daf5153749a35ffb87d13d"
	defaultUser     = "guest"
	defaultPassword = "guest"
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
	managementURL string
	containerName string
	cleanupFn     func()
	initErr       error
	opts          options
)

// Option configures the RabbitMQ test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-rabbitmq-*
// containers before starting new ones.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithImage overrides the default Docker image.
func WithImage(image string) Option {
	return func(o *options) { o.image = image }
}

// WithCredentials overrides the default guest/guest credentials.
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

// Endpoint returns the AMQP 0-9-1 broker URL.
//
// On first call it checks RABBITMQ_URL; if unset it starts a RabbitMQ
// container in Docker. The test is skipped when -short is set or Docker
// is unavailable.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping RabbitMQ integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if url := os.Getenv("RABBITMQ_URL"); url != "" {
			endpoint = url
			fromEnv = true
		} else {
			endpoint, managementURL, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, managementURL, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		t.Skipf("RabbitMQ not available: %v", initErr)
	}
	return endpoint
}

// ManagementURL returns the HTTP management API URL.
func ManagementURL(t testing.TB) string {
	t.Helper()
	_ = Endpoint(t)
	mu.Lock()
	defer mu.Unlock()
	return managementURL
}

// Shutdown stops the RabbitMQ container. Safe to call multiple times.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueQueue returns a queue name with a nanosecond timestamp suffix.
func UniqueQueue(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// UniqueExchange returns an exchange name with a nanosecond timestamp suffix.
func UniqueExchange(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ForceStart resets global state and starts a fresh container.
// Registers t.Cleanup to tear down. Useful for tests that need a
// clean broker.
func ForceStart(t testing.TB) string {
	t.Helper()
	mu.Lock()
	if cleanupFn != nil {
		cleanupFn()
	}
	resolved = false
	fromEnv = false
	endpoint = ""
	managementURL = ""
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

// CreateQueue declares a queue on the RabbitMQ broker via the management API.
func CreateQueue(t testing.TB, name string) {
	t.Helper()
	mgmt := ManagementURL(t)
	body := `{"durable":false,"auto_delete":false}`
	url := fmt.Sprintf("%s/api/queues/%%2F/%s", mgmt, name)

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("rabbitmqlocal: create queue request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user(), password())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rabbitmqlocal: create queue: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rabbitmqlocal: create queue %s: %d %s", name, resp.StatusCode, b)
	}
}

// CreateExchange declares an exchange on the RabbitMQ broker via the management API.
func CreateExchange(t testing.TB, name, kind string) {
	t.Helper()
	mgmt := ManagementURL(t)
	body := fmt.Sprintf(`{"type":%q,"durable":false,"auto_delete":false}`, kind)
	url := fmt.Sprintf("%s/api/exchanges/%%2F/%s", mgmt, name)

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("rabbitmqlocal: create exchange request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user(), password())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rabbitmqlocal: create exchange: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rabbitmqlocal: create exchange %s: %d %s", name, resp.StatusCode, b)
	}
}

// BindQueue binds a queue to an exchange with a routing key via the management API.
func BindQueue(t testing.TB, queue, exchange, routingKey string) {
	t.Helper()
	mgmt := ManagementURL(t)
	body := fmt.Sprintf(`{"routing_key":%q}`, routingKey)
	url := fmt.Sprintf("%s/api/bindings/%%2F/e/%s/q/%s", mgmt, exchange, queue)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("rabbitmqlocal: bind queue request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user(), password())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rabbitmqlocal: bind queue: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rabbitmqlocal: bind %s->%s: %d %s", queue, exchange, resp.StatusCode, b)
	}
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
	mgmtPort, err := dockerexec.FreePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free management port: %w", err)
	}

	name := fmt.Sprintf("%s%d", containerPrefix, amqpPort)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)

	cleanup := func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	}

	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:5672", amqpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:15672", mgmtPort),
		"-e", "RABBITMQ_DEFAULT_USER="+user(),
		"-e", "RABBITMQ_DEFAULT_PASS="+password(),
		imageName(),
	)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	containerName = name

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

	mgmt := fmt.Sprintf("http://127.0.0.1:%d", mgmtPort)
	if err := waitForManagement(mgmt, 60*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("management API: %w", err)
	}

	if err := dockerexec.StabilizeTCP(amqpPort); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("stabilization: %w", err)
	}

	ep := fmt.Sprintf("amqp://%s:%s@127.0.0.1:%d/", user(), password(), amqpPort)
	return ep, mgmt, cleanup, nil
}

// waitForManagement gates on protocol truth: the management healthcheck must
// answer 200 with JSON status "ok" under real authentication — RabbitMQ
// accepts TCP long before the node is actually serviceable.
func waitForManagement(baseURL string, timeout time.Duration) error {
	healthURL := baseURL + "/api/healthchecks/node"
	return dockerexec.WaitProbe("RabbitMQ management at "+baseURL, timeout, time.Second,
		func() error {
			req, _ := http.NewRequest(http.MethodGet, healthURL, nil)
			req.SetBasicAuth(user(), password())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			var result struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&result)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 || result.Status != "ok" {
				return fmt.Errorf("healthcheck status=%d body-status=%q", resp.StatusCode, result.Status)
			}
			return nil
		})
}
