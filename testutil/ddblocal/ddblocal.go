// Package ddblocal provides shared test infrastructure for DynamoDB Local.
//
// It manages the lifecycle of a DynamoDB Local Docker container and
// provides helpers for creating clients, unique table names, and cleanup.
// Multiple test packages in the same binary share a single container
// instance via [Endpoint] / [Client].
//
// The container is automatically restarted if it dies mid-run (e.g. OOM).
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    ddblocal.Configure(ddblocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    ddblocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    client := ddblocal.Client(t)
//	    table  := ddblocal.UniqueTable("my-prefix")
//	    // ... create table, run tests ...
//	    ddblocal.CleanupTable(t, client, table)
//	}
//
// The container is started on first call to [Endpoint] or [Client].
// If the DYNAMODB_ENDPOINT environment variable is set, no container
// is started and that endpoint is used directly (after verifying
// connectivity).
package ddblocal

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const containerPrefix = "gobridge-ddblocal-"

type options struct {
	cleanOrphans bool
	memory       string // e.g. "512m", "1g" — passed to --memory
	cpus         string // e.g. "1.0", "2.0" — passed to --cpus
}

var (
	mu            sync.Mutex
	resolved      bool
	fromEnv       bool
	endpoint      string
	containerName string
	cleanupFn     func()
	initErr       error
	opts          options
)

// Option configures the DynamoDB Local test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-ddblocal-*
// containers before starting a new one. Recommended for CI and long
// test suites to prevent resource leaks from crashed runs.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithMemory sets the Docker --memory limit for the container (e.g. "512m", "1g").
func WithMemory(limit string) Option {
	return func(o *options) { o.memory = limit }
}

// WithCPUs sets the Docker --cpus limit for the container (e.g. "1.0", "2.0").
func WithCPUs(limit string) Option {
	return func(o *options) { o.cpus = limit }
}

// Configure applies options before the container is started.
// Must be called before the first [Endpoint] or [Client] call.
// Calling after the container is running has no effect.
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

// Endpoint returns the DynamoDB Local endpoint URL.
//
// On first call it checks DYNAMODB_ENDPOINT; if unset it starts a
// DynamoDB Local Docker container. The test is skipped when -short is
// set or Docker is unavailable.
//
// On subsequent calls the container health is verified. If the container
// died it is restarted transparently (tables are lost since the container
// runs in-memory).
//
// Call [Shutdown] in TestMain after m.Run() to stop the container.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DynamoDB integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("DYNAMODB_ENDPOINT"); ep != "" {
			endpoint = ep
			fromEnv = true
			initErr = waitForServiceReady(ep, 10*time.Second)
		} else {
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !isContainerRunning(containerName) {
			t.Logf("ddblocal: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		t.Skipf("DynamoDB Local not available: %v", initErr)
	}
	return endpoint
}

// Client returns a *dynamodb.Client connected to DynamoDB Local.
// Same skip/restart semantics as [Endpoint].
func Client(t testing.TB) *dynamodb.Client {
	t.Helper()
	ep := Endpoint(t)
	return newClient(ep)
}

// Shutdown stops the DynamoDB Local container if one was started.
// Safe to call multiple times or when no container was started.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// ForceStart kills any existing container and starts a fresh DynamoDB Local
// container. The container is removed when the test ends via t.Cleanup.
// Returns the endpoint URL.
//
// Use this instead of [Endpoint] when the test needs a guaranteed-fresh
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

	removeOrphans(containerPrefix)

	ep, name, cleanup, err := startContainer()
	if err != nil {
		t.Fatalf("ddblocal.ForceStart: %v", err)
	}

	endpoint = ep
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

	return ep
}

// UniqueTable returns a table name built from prefix and a nanosecond
// timestamp, suitable for test isolation.
func UniqueTable(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// CleanupTable registers a t.Cleanup that deletes the table when the
// test (or subtest) finishes.
func CleanupTable(t testing.TB, client *dynamodb.Client, tableName string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{
			TableName: aws.String(tableName),
		})
	})
}

// WaitUntilReady verifies that the DynamoDB Local endpoint is responding.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	ep := Endpoint(t)
	if err := waitForServiceReady(ep, 30*time.Second); err != nil {
		t.Fatalf("ddblocal: %v", err)
	}
}

func newClient(ep string) *dynamodb.Client {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-west-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(ep)
	})
}

// --- container lifecycle ---

func startContainer() (string, string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", "", nil, fmt.Errorf("docker not found: %w", err)
	}

	if opts.cleanOrphans {
		removeOrphans(containerPrefix)
	}

	port, err := freePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free port: %w", err)
	}

	name := containerPrefix + fmt.Sprintf("%d", port)

	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)

	args := []string{"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:8000", port),
	}
	if opts.memory != "" {
		args = append(args, "--memory", opts.memory)
	}
	if opts.cpus != "" {
		args = append(args, "--cpus", opts.cpus)
	}
	args = append(args, "amazon/dynamodb-local:latest",
		"-jar", "DynamoDBLocal.jar", "-sharedDb", "-inMemory")
	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	ep := fmt.Sprintf("http://127.0.0.1:%d", port)
	cleanup := func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	}

	if err := waitForContainerHealthy(name, 15*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := waitForServiceReady(ep, 30*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := stabilize(func() error {
		_, e := newClient(ep).ListTables(context.Background(), &dynamodb.ListTablesInput{})
		return e
	}); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("DynamoDB Local stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
}

// removeOrphans kills and removes all containers whose name starts with
// the given prefix. This cleans up leftovers from crashed test runs.
func removeOrphans(prefix string) {
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "ps", "-aq",
		"--filter", "name="+prefix)
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, args...)
	}
}

func isContainerRunning(name string) bool {
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect",
		"--format", "{{.State.Running}}", name)
	return err == nil && len(out) > 0 && out[0] == 't'
}

func waitForContainerHealthy(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect",
			"--format", "{{.State.Running}} {{.State.ExitCode}}", name)
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

func waitForServiceReady(ep string, timeout time.Duration) error {
	client := newClient(ep)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = client.ListTables(context.Background(), &dynamodb.ListTablesInput{})
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("DynamoDB Local at %s did not become ready within %v: %v", ep, timeout, lastErr)
}

func stabilize(probe func() error) error {
	for i := 0; i < 3; i++ {
		if err := probe(); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func logContainerFailure(name string) {
	out, _ := dockerexec.Run(dockerexec.LogsTimeout, "logs", "--tail", "30", name)
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
