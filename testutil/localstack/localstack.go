// Package localstack provides shared test infrastructure for LocalStack,
// an AWS cloud service emulator running in Docker.
//
// Unlike the single-service helpers (ddblocal, sqslocal, etc.), this
// package runs a single LocalStack container that emulates multiple
// AWS services simultaneously: SSM Parameter Store, CloudWatch, and
// any other service supported by the LocalStack Community image.
//
// Multiple test packages in the same binary share a single container
// instance via [Endpoint] and the typed client helpers.
//
// The container is automatically restarted if it dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    localstack.Configure(
//	        localstack.WithServices("ssm", "cloudwatch"),
//	        localstack.WithCleanOrphans(true),
//	    )
//	    code := m.Run()
//	    localstack.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSSM(t *testing.T) {
//	    client := localstack.SSMClient(t)
//	    // ... put/get parameters ...
//	}
//
//	func TestCloudWatch(t *testing.T) {
//	    client := localstack.CloudWatchClient(t)
//	    // ... put metric data, put metric alarms ...
//	}
//
// The container is started on first call to [Endpoint] or any client
// helper. If the LOCALSTACK_ENDPOINT environment variable is set, no
// container is started and that endpoint is used directly (after
// verifying connectivity).
package localstack

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const containerPrefix = "gobridge-localstack-"

type options struct {
	cleanOrphans bool
	services     []string
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

// Option configures the LocalStack test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-localstack-*
// containers before starting a new one. Recommended for CI and long
// test suites to prevent resource leaks from crashed runs.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithServices specifies which AWS services LocalStack should activate.
// Example: "ssm", "cloudwatch", "s3". When empty, LocalStack starts
// with its default service set.
func WithServices(services ...string) Option {
	return func(o *options) { o.services = services }
}

// Configure applies options before the container is started.
// Must be called before the first [Endpoint] or client call.
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

// Endpoint returns the LocalStack gateway endpoint URL.
//
// On first call it checks LOCALSTACK_ENDPOINT; if unset it starts a
// LocalStack Docker container. The test is skipped when -short is set
// or Docker is unavailable.
//
// On subsequent calls the container health is verified. If the container
// died it is restarted transparently.
//
// Call [Shutdown] in TestMain after m.Run() to stop the container.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping LocalStack integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("LOCALSTACK_ENDPOINT"); ep != "" {
			endpoint = ep
			fromEnv = true
			initErr = waitForServiceReady(ep, 10*time.Second)
		} else {
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !isContainerRunning(containerName) {
			t.Logf("localstack: container %s died, restarting...", containerName)
			if cleanupFn != nil {
				cleanupFn()
			}
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		t.Skipf("LocalStack not available: %v", initErr)
	}
	return endpoint
}

// AWSConfig returns an aws.Config pointed at the LocalStack endpoint
// with static test credentials. Use this to build clients for any AWS
// service that LocalStack supports.
func AWSConfig(t testing.TB) aws.Config {
	t.Helper()
	ep := Endpoint(t)
	return newAWSConfig(ep)
}

// SSMClient returns an *ssm.Client connected to LocalStack.
// Same skip/restart semantics as [Endpoint].
func SSMClient(t testing.TB) *ssm.Client {
	t.Helper()
	ep := Endpoint(t)
	return ssm.NewFromConfig(newAWSConfig(ep), func(o *ssm.Options) {
		o.BaseEndpoint = aws.String(ep)
	})
}

// CloudWatchClient returns a *cloudwatch.Client connected to LocalStack.
// Same skip/restart semantics as [Endpoint].
func CloudWatchClient(t testing.TB) *cloudwatch.Client {
	t.Helper()
	ep := Endpoint(t)
	return cloudwatch.NewFromConfig(newAWSConfig(ep), func(o *cloudwatch.Options) {
		o.BaseEndpoint = aws.String(ep)
	})
}

// Shutdown stops the LocalStack container if one was started and
// removes any orphaned gobridge-localstack-* containers left behind
// by previous crashed runs.
// Safe to call multiple times or when no container was started.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
	removeOrphans(containerPrefix)
}

// WaitUntilReady verifies that the LocalStack endpoint is responding.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	ep := Endpoint(t)
	if err := waitForServiceReady(ep, 60*time.Second); err != nil {
		t.Fatalf("localstack: %v", err)
	}
}

func newAWSConfig(ep string) aws.Config {
	cfg, _ := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-west-1"),
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
		removeOrphans(containerPrefix)
	}

	port, err := freePort()
	if err != nil {
		return "", "", nil, fmt.Errorf("find free port: %w", err)
	}

	name := containerPrefix + fmt.Sprintf("%d", port)
	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := []string{
		"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:4566", port),
		"-e", "LOCALSTACK_ACKNOWLEDGE_ACCOUNT_REQUIREMENT=1",
	}
	if len(opts.services) > 0 {
		args = append(args, "-e", "SERVICES="+strings.Join(opts.services, ","))
	}
	if token := os.Getenv("LOCALSTACK_AUTH_TOKEN"); token != "" {
		args = append(args, "-e", "LOCALSTACK_AUTH_TOKEN="+token)
	}
	args = append(args, "localstack/localstack:latest")

	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	ep := fmt.Sprintf("http://127.0.0.1:%d", port)
	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}

	if err := waitForContainerHealthy(name, 30*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := waitForServiceReady(ep, 60*time.Second); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, err
	}

	if err := stabilize(func() error {
		resp, e := http.Get(ep + "/_localstack/health")
		if e != nil {
			return e
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		return nil
	}); err != nil {
		logContainerFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("LocalStack stabilization failed: %w", err)
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

// waitForServiceReady polls the LocalStack /_localstack/health endpoint
// until all requested services report "available" or "running".
func waitForServiceReady(ep string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(ep + "/_localstack/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("LocalStack at %s did not become ready within %v: %v", ep, timeout, lastErr)
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
