// Package s3local provides shared test infrastructure for a MinIO
// S3-compatible service running in Docker.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    s3local.Configure(s3local.WithCleanOrphans(true))
//	    code := m.Run()
//	    s3local.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    client := s3local.Client(t)
//	    bucket := s3local.UniqueBucket("my-prefix")
//	    s3local.CreateBucket(t, client, bucket)
//	    // ... put/get objects ...
//	}
package s3local

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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const (
	containerPrefix = "gobridge-s3local-"
	// defaultImage is pinned by digest, like rabbitmqlocal: a floating
	// :latest means CI can break on a day nobody changed anything.
	defaultImage    = "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"
	defaultUser     = "minioadmin"
	defaultPassword = "minioadmin"
)

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

// Option configures the MinIO test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-s3local-*
// containers before starting a new one.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// Configure applies options before the container is started.
// Must be called before the first [Endpoint] or [Client] call.
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

// Endpoint returns the MinIO S3 endpoint URL.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping S3 integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
			endpoint = ep
			fromEnv = true
			initErr = waitForServiceReady(ep, 10*time.Second)
		} else {
			endpoint, containerName, cleanupFn, initErr = startContainer()
		}
	} else if !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			t.Logf("s3local: container %s died, restarting...", containerName)
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
			t.Fatalf("MinIO not available: %v", initErr)
		}
		t.Skipf("MinIO not available (docker absent): %v", initErr)
	}
	return endpoint
}

// Client returns a *s3.Client connected to MinIO.
func Client(t testing.TB) *s3.Client {
	t.Helper()
	return newClient(Endpoint(t))
}

// Shutdown stops the MinIO container if one was started.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueBucket returns a bucket name with a nanosecond timestamp suffix.
func UniqueBucket(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// CreateBucket creates an S3 bucket and registers a t.Cleanup to delete it.
func CreateBucket(t testing.TB, client *s3.Client, name string) {
	t.Helper()
	_, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("s3local: create bucket %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteBucket(context.Background(), &s3.DeleteBucketInput{
			Bucket: aws.String(name),
		})
	})
}

// WaitUntilReady verifies that the MinIO endpoint is responding.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	ep := Endpoint(t)
	if err := waitForServiceReady(ep, 30*time.Second); err != nil {
		t.Fatalf("s3local: %v", err)
	}
}

func newClient(ep string) *s3.Client {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-west-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(defaultUser, defaultPassword, ""),
		),
	)
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ep)
		o.UsePathStyle = true
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

	if err := dockerexec.EnsureImage(defaultImage); err != nil {
		return "", "", nil, err
	}

	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER="+defaultUser,
		"-e", "MINIO_ROOT_PASSWORD="+defaultPassword,
		defaultImage,
		"server", "/data",
	)
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

	if err := dockerexec.Stabilize(healthLive(ep)); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", nil, fmt.Errorf("MinIO stabilization failed: %w", err)
	}

	return ep, name, cleanup, nil
}

// healthLive probes MinIO's liveness endpoint — protocol truth beyond a bare
// TCP accept.
func healthLive(ep string) func() error {
	return func() error {
		resp, err := http.Get(ep + "/minio/health/live")
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
	return dockerexec.WaitProbe("MinIO at "+ep, timeout, 500*time.Millisecond, healthLive(ep))
}
