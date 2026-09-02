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
	"os"
	"os/exec"
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

// defaultImage is pinned by digest, like rabbitmqlocal: a floating :latest
// means CI can break on a day nobody changed anything.
const defaultImage = "amazon/dynamodb-local:3.3.0@sha256:d89f8fcc6b1a39cb35976c248ed42a28c66ae00dc043099210f5571e42648ab4"

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
		if !dockerexec.IsRunning(containerName) {
			t.Logf("ddblocal: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		// A fixture that will not start is a failure wherever it could have
		// started; skipping here is what let a permanently broken emulator
		// report `ok` for its whole package. See dockerexec.MustSucceed.
		if dockerexec.MustSucceed() {
			t.Fatalf("DynamoDB Local not available: %v", initErr)
		}
		t.Skipf("DynamoDB Local not available (docker absent): %v", initErr)
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

// ContainerName returns the name of the container this process started, so a
// caller that has to reach DynamoDB Local from ANOTHER container can attach it
// to a Docker network by name. It is empty when DYNAMODB_ENDPOINT pointed the
// tests at an externally managed instance, which the caller must handle: there
// is no container here to attach.
//
// Same start/skip semantics as [Endpoint].
func ContainerName(t testing.TB) string {
	t.Helper()
	_ = Endpoint(t)
	mu.Lock()
	defer mu.Unlock()
	return containerName
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

	dockerexec.RemoveOrphans(containerPrefix)

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
		dockerexec.RemoveOrphans(containerPrefix)
	}

	port, err := dockerexec.FreePort()
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
	args = append(args, defaultImage,
		"-jar", "DynamoDBLocal.jar", "-sharedDb", "-inMemory")
	if err := dockerexec.EnsureImage(defaultImage); err != nil {
		return "", "", nil, err
	}

	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	ep := fmt.Sprintf("http://127.0.0.1:%d", port)
	cleanup := func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	}

	if err := dockerexec.WaitHealthy(name, 15*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := waitForServiceReady(ep, 30*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := dockerexec.Stabilize(func() error {
		_, e := newClient(ep).ListTables(context.Background(), &dynamodb.ListTablesInput{})
		return e
	}); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("DynamoDB Local stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
}

// waitForServiceReady gates on protocol truth: DynamoDB Local is ready when
// it answers a real ListTables call, not merely when its port accepts TCP.
func waitForServiceReady(ep string, timeout time.Duration) error {
	client := newClient(ep)
	return dockerexec.WaitProbe("DynamoDB Local at "+ep, timeout, 500*time.Millisecond,
		func() error {
			_, err := client.ListTables(context.Background(), &dynamodb.ListTablesInput{})
			return err
		})
}
