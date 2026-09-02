// Package flocilocal provides shared test infrastructure for Floci, an
// open-source AWS emulator that serves every AWS API the test suite needs on
// one endpoint.
//
// It replaces the per-service emulator wrappers the suite used to carry (one
// for SQS, one for S3, one for a multi-service emulator behind a licence
// token). Tests that need SQS, SSM, CloudWatch or any other emulated AWS API
// build their client from [AWSConfig] and talk to the same container.
//
// DynamoDB is deliberately NOT served from here: `testutil/ddblocal` stays on
// Amazon's own DynamoDB Local, because the store adapters are compare-and-swap
// end to end and conditional-write semantics have to be the reference ones.
// Message brokers are not served from here either — Floci emulates AWS APIs,
// not MQTT, AMQP or Azure Service Bus.
//
// Multiple test packages in the same binary share a single container instance
// via [Endpoint] / [AWSConfig]. The container is automatically restarted if it
// dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    flocilocal.Configure(flocilocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    flocilocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    client := sqs.NewFromConfig(flocilocal.AWSConfig(t))
//	    // ... send/receive messages ...
//	}
//
// The container is started on first call to [Endpoint] or [AWSConfig].
// If the FLOCI_ENDPOINT environment variable is set, no container is started
// and that endpoint is used directly (after verifying connectivity).
package flocilocal

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const containerPrefix = "gobridge-flocilocal-"

// defaultImage is pinned by digest, like the other container helpers: a
// floating :latest means CI can break on a day nobody changed anything.
const defaultImage = "floci/floci:2.0.1@sha256:4e451c39c7bb88e3cd4f87e8fc0c25d5b47695a51185d521e2241fa00486e8eb"

// gatewayPort is the single port every emulated AWS API is served on.
const gatewayPort = 4566

// Region is the region every client built by [AWSConfig] signs for. The
// emulator accepts any region; a named constant keeps the migrated tests from
// each carrying their own string.
const Region = "us-west-1"

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

// Option configures the Floci test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-flocilocal-*
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
// Must be called before the first [Endpoint] or [AWSConfig] call.
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

// Endpoint returns the Floci gateway endpoint URL, which serves every
// emulated AWS API.
//
// On first call it checks FLOCI_ENDPOINT; if unset it starts a Floci Docker
// container. The test is skipped when -short is set or Docker is unavailable.
//
// On subsequent calls the container health is verified. If the container died
// it is restarted transparently (emulator state is lost — it is in-memory).
//
// Call [Shutdown] in TestMain after m.Run() to stop the container.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping AWS emulator integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("FLOCI_ENDPOINT"); ep != "" {
			endpoint = ep
			fromEnv = true
			initErr = waitForServiceReady(ep, 10*time.Second)
		} else {
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			t.Logf("flocilocal: container %s died, restarting...", containerName)
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
			t.Fatalf("Floci not available: %v", initErr)
		}
		t.Skipf("Floci not available (docker absent): %v", initErr)
	}
	return endpoint
}

// AWSConfig returns an aws.Config pointed at the Floci endpoint with static
// test credentials. Use it to build a client for any emulated AWS service:
//
//	sqs.NewFromConfig(flocilocal.AWSConfig(t))
//
// Same skip/restart semantics as [Endpoint].
func AWSConfig(t testing.TB) aws.Config {
	t.Helper()
	return newAWSConfig(Endpoint(t))
}

// Shutdown stops the Floci container this process started, if any. Safe to
// call multiple times or when no container was started.
//
// It deliberately stops only its own container and never sweeps the whole
// gobridge-flocilocal- prefix: a test that re-execs the test binary as a child
// process hands the child FLOCI_ENDPOINT so both share one emulator, and a
// prefix sweep on the child's exit would tear down the emulator its parent is
// still using. Leftovers from a crashed run are cleared by
// [WithCleanOrphans] at start, which is where that concern belongs.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// ForceStart kills any existing container and starts a fresh Floci container.
// The container is removed when the test ends via t.Cleanup. Returns the
// endpoint URL.
//
// Use this instead of [Endpoint] when the test needs a guaranteed-fresh
// emulator (e.g. resilience or restart tests).
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
		t.Fatalf("flocilocal.ForceStart: %v", err)
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

func newAWSConfig(ep string) aws.Config {
	cfg, _ := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	cfg.BaseEndpoint = aws.String(ep)
	return cfg
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

	// The Docker socket is deliberately NOT bind-mounted. Floci needs it only
	// to run ECS tasks and Lambda functions as real containers; nothing here
	// does, and handing every test binary the host daemon is a privilege the
	// suite has no use for.
	args := []string{
		"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, gatewayPort),
	}
	if opts.memory != "" {
		args = append(args, "--memory", opts.memory)
	}
	if opts.cpus != "" {
		args = append(args, "--cpus", opts.cpus)
	}
	args = append(args, defaultImage)

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

	if err := dockerexec.WaitHealthy(name, 30*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := waitForServiceReady(ep, 60*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := dockerexec.Stabilize(healthOK(ep)); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("floci stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
}

// probeClient bounds every health request. dockerexec.WaitProbe checks its
// deadline only between probes, so an unbounded request against a container
// that accepts TCP and then never answers would hang well past the timeout the
// caller asked for.
var probeClient = &http.Client{Timeout: 2 * time.Second}

// healthOK probes the Floci health endpoint for HTTP 200 — the gateway answers
// only once its service manager is up. (Per-service states are not parsed; the
// stabilize pass plus each test's own first API call cover that.)
func healthOK(ep string) func() error {
	return func() error {
		resp, err := probeClient.Get(ep + "/_floci/health")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		return nil
	}
}

func waitForServiceReady(ep string, timeout time.Duration) error {
	return dockerexec.WaitProbe("Floci at "+ep, timeout, 500*time.Millisecond, healthOK(ep))
}
