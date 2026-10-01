// Package artemislocal provides shared test infrastructure for an
// Apache ActiveMQ Artemis broker running in Docker.
//
// It manages a single Artemis container and provides helpers for
// creating AMQP 1.0 clients and addresses. Multiple test packages
// in the same binary share a single container via [Endpoint].
//
// The container is automatically restarted if it dies mid-run.
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    artemislocal.Configure(artemislocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    artemislocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    ep := artemislocal.Endpoint(t) // "amqp://127.0.0.1:<port>"
//	    // ... create sender/receiver with endpoint ...
//	}
//
// The container is started on first call to [Endpoint].
// If the ARTEMIS_URL environment variable is set, no container is
// started and that URL is used directly.
//
// [WithTLS] adds an AMQP 1.0 TLS listener served by a generated certificate
// authority ([TLSEndpoint], [CAPEM]). [WithNetwork] attaches the container to a
// Docker network so a client in another container reaches it by name
// ([NetworkEndpoint], [NetworkTLSEndpoint]).
package artemislocal

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

const (
	containerPrefix = "gobridge-artemis-"
	defaultImage    = "apache/activemq-artemis:2.44.0-alpine@sha256:1bce124d2324faeb1253e2db4bb68d64decf46412ecfd8d9ef46c08b1b4af5b4"
	defaultUser     = "admin"
	defaultPassword = "admin"

	// amqpContainerPort is the plaintext AMQP 1.0 acceptor inside the container.
	amqpContainerPort = 5672
)

type options struct {
	cleanOrphans bool
	image        string
	user         string
	password     string
	tls          bool
	network      string
}

// addresses is every address the running fixture serves. An address whose
// option was not given stays empty.
type addresses struct {
	endpoint           string
	consoleURL         string
	tlsEndpoint        string
	networkEndpoint    string
	networkTLSEndpoint string
	caPEM              string
}

var (
	mu            sync.Mutex
	resolved      bool
	fromEnv       bool
	current       addresses
	containerName string
	cleanupFn     func()
	initErr       error
	opts          options
)

// Option configures the Artemis test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-artemis-*
// containers before starting new ones.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithImage overrides the default Docker image.
func WithImage(image string) Option {
	return func(o *options) { o.image = image }
}

// WithCredentials overrides the default admin/admin credentials.
func WithCredentials(user, password string) Option {
	return func(o *options) { o.user = user; o.password = password }
}

// WithNetwork attaches the container to the named Docker network, so a client
// in another container on that network reaches the broker by its container
// name ([NetworkEndpoint], [NetworkTLSEndpoint]). The ports are still
// published on 127.0.0.1 for the test process.
//
// The caller creates the network before the broker starts and removes it after
// the broker is gone; this package does neither.
func WithNetwork(network string) Option {
	return func(o *options) { o.network = network }
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

// Endpoint returns the AMQP 1.0 broker URL.
//
// On first call it checks ARTEMIS_URL; if unset it starts an Artemis
// container in Docker. The test is skipped when -short is set or Docker
// is unavailable.
func Endpoint(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Artemis integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if url := os.Getenv("ARTEMIS_URL"); url != "" {
			current = addresses{endpoint: url}
			fromEnv = true
		} else {
			current, cleanupFn, initErr = startContainer()
		}
	} else if initErr == nil && !fromEnv && containerName != "" {
		if !dockerexec.IsRunning(containerName) {
			if cleanupFn != nil {
				cleanupFn()
			}
			current, cleanupFn, initErr = startContainer()
		}
	}

	if initErr != nil {
		// A fixture that will not start is a failure wherever it could have
		// started; skipping here is what let a permanently broken emulator
		// report `ok` for its whole package. See dockerexec.MustSucceed.
		if dockerexec.MustSucceed() {
			t.Fatalf("Artemis not available: %v", initErr)
		}
		t.Skipf("Artemis not available (docker absent): %v", initErr)
	}
	return current.endpoint
}

// ConsoleURL returns the Artemis web console URL.
func ConsoleURL(t testing.TB) string {
	t.Helper()
	return address(t, func(a addresses) string { return a.consoleURL })
}

// NetworkEndpoint returns the plaintext AMQP 1.0 address a client on the Docker
// network uses (amqp://<container name>:5672). It is empty when the package
// was not configured [WithNetwork], or when ARTEMIS_URL pointed it at a broker
// it did not start.
//
// Same start/skip semantics as [Endpoint].
func NetworkEndpoint(t testing.TB) string {
	t.Helper()
	return address(t, func(a addresses) string { return a.networkEndpoint })
}

// address starts the broker like [Endpoint] does, then reads one of the
// addresses it serves.
func address(t testing.TB, field func(addresses) string) string {
	t.Helper()
	_ = Endpoint(t)
	mu.Lock()
	defer mu.Unlock()
	return field(current)
}

// Credentials returns the configured username and password.
func Credentials() (string, string) {
	return user(), password()
}

// Shutdown stops the Artemis container. Safe to call multiple times.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueAddress returns an address name with a nanosecond timestamp suffix.
func UniqueAddress(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ForceStart resets global state and starts a fresh container.
// Registers t.Cleanup to tear down.
func ForceStart(t testing.TB) string {
	t.Helper()
	mu.Lock()
	if cleanupFn != nil {
		cleanupFn()
	}
	resolved = false
	fromEnv = false
	current = addresses{}
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

func startContainer() (addresses, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return addresses{}, nil, fmt.Errorf("docker not found: %w", err)
	}

	if opts.cleanOrphans {
		dockerexec.RemoveOrphans(containerPrefix)
	}

	amqpPort, err := dockerexec.FreePort()
	if err != nil {
		return addresses{}, nil, fmt.Errorf("find free AMQP port: %w", err)
	}
	webPort, err := dockerexec.FreePort()
	if err != nil {
		return addresses{}, nil, fmt.Errorf("find free web port: %w", err)
	}
	var tlsPort int
	if opts.tls {
		if tlsPort, err = dockerexec.FreePort(); err != nil {
			return addresses{}, nil, fmt.Errorf("find free AMQP TLS port: %w", err)
		}
	}

	name := fmt.Sprintf("%s%d", containerPrefix, amqpPort)
	_, _ = dockerexec.Remove(name)

	// Written before the container starts: the image copies it into the broker
	// configuration when it creates the broker instance.
	var started addresses
	var materialDir string
	if opts.tls {
		if materialDir, started.caPEM, err = writeTLSMaterial(name); err != nil {
			return addresses{}, nil, err
		}
	}

	// Every failure below runs this too, so a fixture that could not start
	// leaves neither a container nor a material directory behind.
	cleanup := func() {
		_, _ = dockerexec.Remove(name)
		if materialDir != "" {
			_ = os.RemoveAll(materialDir)
		}
	}

	if err := dockerexec.EnsureImage(imageName()); err != nil {
		cleanup()
		return addresses{}, nil, err
	}

	args := []string{"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", amqpPort, amqpContainerPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:8161", webPort),
		"-e", "ARTEMIS_USER=" + user(),
		"-e", "ARTEMIS_PASSWORD=" + password(),
		"-e", "EXTRA_ARGS=--relax-jolokia",
	}
	if opts.tls {
		args = append(args,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", tlsPort, tlsContainerPort),
			"-v", materialDir+":"+overrideMountPath+":ro")
	}
	if opts.network != "" {
		args = append(args, "--network", opts.network)
	}
	out, err := dockerexec.Run(dockerexec.RunTimeout, append(args, imageName())...)
	if err != nil {
		cleanup()
		return addresses{}, nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}

	if err := dockerexec.WaitHealthy(name, 30*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return addresses{}, nil, fmt.Errorf("container: %w", err)
	}

	if err := dockerexec.WaitTCP(amqpPort, 60*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return addresses{}, nil, fmt.Errorf("AMQP port: %w", err)
	}

	if err := dockerexec.StabilizeTCP(amqpPort); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return addresses{}, nil, fmt.Errorf("stabilization: %w", err)
	}

	// Protocol truth: Artemis accepts TCP well before its AMQP acceptor
	// authenticates — gate on a real SASL dial, not the socket.
	amqpEP := fmt.Sprintf("amqp://127.0.0.1:%d", amqpPort)
	if err := dockerexec.WaitProbe("Artemis AMQP on "+amqpEP, 30*time.Second, time.Second,
		amqpProbe(amqpEP, nil)); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return addresses{}, nil, err
	}

	if opts.tls {
		started.tlsEndpoint = fmt.Sprintf("amqps://127.0.0.1:%d", tlsPort)
		if err := waitTLSReady(started.tlsEndpoint, started.caPEM); err != nil {
			dockerexec.LogFailure(name)
			cleanup()
			return addresses{}, nil, err
		}
	}
	if opts.network != "" {
		started.networkEndpoint = fmt.Sprintf("amqp://%s:%d", name, amqpContainerPort)
		if opts.tls {
			started.networkTLSEndpoint = fmt.Sprintf("amqps://%s:%d", name, tlsContainerPort)
		}
	}

	containerName = name
	started.endpoint = amqpEP
	started.consoleURL = fmt.Sprintf("http://127.0.0.1:%d", webPort)
	return started, cleanup, nil
}

// amqpProbe gates on a real AMQP 1.0 SASL dial — success proves the broker
// authenticates and speaks the protocol, not merely that the port is open.
// A nil tlsConfig dials a plaintext endpoint.
func amqpProbe(ep string, tlsConfig *tls.Config) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		conn, err := amqp.Dial(ctx, ep, &amqp.ConnOptions{
			SASLType:  amqp.SASLTypePlain(user(), password()),
			TLSConfig: tlsConfig,
		})
		if err != nil {
			return err
		}
		return conn.Close()
	}
}
