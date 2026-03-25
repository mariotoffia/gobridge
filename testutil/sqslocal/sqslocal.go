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
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const containerPrefix = "gobridge-sqslocal-"

type options struct {
	cleanOrphans bool
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
		if !isContainerRunning(containerName) {
			t.Logf("sqslocal: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		t.Skipf("ElasticMQ not available: %v", initErr)
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
		config.WithRegion("us-east-1"),
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
		removeOrphans(containerPrefix)
	}

	port, err := freePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free port: %w", err)
	}

	name := containerPrefix + fmt.Sprintf("%d", port)
	_ = exec.Command("docker", "rm", "-f", name).Run()

	cmd := exec.Command("docker", "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:9324", port),
		"softwaremill/elasticmq-native:latest",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	ep := fmt.Sprintf("http://127.0.0.1:%d", port)
	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
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
		_, e := newClient(ep).ListQueues(context.Background(), &sqs.ListQueuesInput{})
		return e
	}); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("ElasticMQ stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
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

func waitForServiceReady(ep string, timeout time.Duration) error {
	client := newClient(ep)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = client.ListQueues(context.Background(), &sqs.ListQueuesInput{})
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("ElasticMQ at %s did not become ready within %v: %v", ep, timeout, lastErr)
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
