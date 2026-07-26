// Package sqslocal provides shared test infrastructure for an ElasticMQ
// SQS-compatible service running in Docker.
//
// It manages the lifecycle of an ElasticMQ container and provides helpers
// for creating clients, queues, and cleanup. Multiple test packages in the
// same binary share a single container instance via [Endpoint] / [Client].
//
// The container is automatically restarted if it dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    sqslocal.Configure(sqslocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    sqslocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    client := sqslocal.Client(t)
//	    queueURL := sqslocal.CreateQueue(t, client, sqslocal.UniqueQueue("my-prefix"))
//	    // ... send/receive messages ...
//	}
//
// The container is started on first call to [Endpoint] or [Client].
// If the SQS_ENDPOINT environment variable is set, no container is
// started and that endpoint is used directly (after verifying
// connectivity).
package sqslocal

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
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const containerPrefix = "gobridge-sqslocal-"

// defaultImage is pinned by digest, like rabbitmqlocal: a floating :latest
// means CI can break on a day nobody changed anything.
const defaultImage = "softwaremill/elasticmq-native:1.7.1@sha256:e4580ab9ad1bd5cd37b4ba04911bc5ccc8cd2d9ab4de56ece65acee71c24e05c"

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

// Option configures the ElasticMQ test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-sqslocal-*
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

// Endpoint returns the ElasticMQ SQS endpoint URL.
//
// On first call it checks SQS_ENDPOINT; if unset it starts an ElasticMQ
// Docker container. On subsequent calls the container health is verified
// and it is restarted if it died.
//
// Call [Shutdown] in TestMain after m.Run() to stop the container.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping SQS integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("SQS_ENDPOINT"); ep != "" {
			endpoint = ep
			fromEnv = true
			initErr = waitForServiceReady(ep, 10*time.Second)
		} else {
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			t.Logf("sqslocal: container %s died, restarting...", containerName)
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
			t.Fatalf("ElasticMQ not available: %v", initErr)
		}
		t.Skipf("ElasticMQ not available (docker absent): %v", initErr)
	}
	return endpoint
}

// Client returns a *sqs.Client connected to ElasticMQ.
func Client(t testing.TB) *sqs.Client {
	t.Helper()
	ep := Endpoint(t)
	return newClient(ep)
}

// Shutdown stops the ElasticMQ container if one was started.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// ForceStart kills any existing container and starts a fresh ElasticMQ
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
		t.Fatalf("sqslocal.ForceStart: %v", err)
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

// UniqueQueue returns a queue name with a nanosecond timestamp suffix.
func UniqueQueue(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// CreateQueue creates an SQS queue and registers a t.Cleanup to delete it.
func CreateQueue(t testing.TB, client *sqs.Client, name string) string {
	t.Helper()
	out, err := client.CreateQueue(context.Background(), &sqs.CreateQueueInput{
		QueueName: aws.String(name),
	})
	if err != nil {
		t.Fatalf("sqslocal: create queue %q: %v", name, err)
	}
	queueURL := *out.QueueUrl
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		})
	})
	return queueURL
}

// CreateQueueWithAttrs creates an SQS queue with custom attributes.
func CreateQueueWithAttrs(t testing.TB, client *sqs.Client, name string, attrs map[string]string) string {
	t.Helper()
	out, err := client.CreateQueue(context.Background(), &sqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: attrs,
	})
	if err != nil {
		t.Fatalf("sqslocal: create queue %q: %v", name, err)
	}
	queueURL := *out.QueueUrl
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		})
	})
	return queueURL
}

// WaitUntilReady verifies that the ElasticMQ endpoint is responding.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	ep := Endpoint(t)
	if err := waitForServiceReady(ep, 30*time.Second); err != nil {
		t.Fatalf("sqslocal: %v", err)
	}
}

func newClient(ep string) *sqs.Client {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-west-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
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
		"-p", fmt.Sprintf("127.0.0.1:%d:9324", port),
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
		_, e := newClient(ep).ListQueues(context.Background(), &sqs.ListQueuesInput{})
		return e
	}); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("ElasticMQ stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
}

// waitForServiceReady gates on protocol truth: ElasticMQ is ready when it
// answers a real ListQueues call, not merely when its port accepts TCP.
func waitForServiceReady(ep string, timeout time.Duration) error {
	client := newClient(ep)
	return dockerexec.WaitProbe("ElasticMQ at "+ep, timeout, 500*time.Millisecond,
		func() error {
			_, err := client.ListQueues(context.Background(), &sqs.ListQueuesInput{})
			return err
		})
}
