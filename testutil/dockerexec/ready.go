package dockerexec

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Readiness and teardown gates shared by every testutil/*local launcher.
//
// Deterministic-gate model (TESTS.md §2.1): launchers and tests never sleep to
// "give things time" — they wait ON an observable state (container running,
// port accepting, protocol probe succeeding, container stopped/gone) with time
// only as poll pacing and failure budget. This package and testutil/wait are
// the two sanctioned homes for that pacing; `make audit-test-timings` enforces
// that every other testutil package stays sleep-free.

const (
	pollInterval      = 200 * time.Millisecond
	stabilizeAttempts = 3
	stabilizeInterval = 100 * time.Millisecond
)

// PullTimeout bounds an explicit image pull. It is deliberately far larger
// than RunTimeout: pulling SQL Server or LocalStack on a cold cache moves
// well over a gigabyte.
const PullTimeout = 15 * time.Minute

// EnsureImage makes sure ref is present locally, pulling it if it is not.
//
// Call this before `docker run`. Otherwise the run performs an implicit pull
// inside its own (short) timeout, so the fixture succeeds on a warm cache and
// fails on a cold one — a fresh CI runner pulls every image for the first
// time, and a multi-gigabyte image will not arrive inside RunTimeout. That is
// a timing-dependent failure disguised as a container fault, so separating
// "fetch the image" from "start the container" makes startup deterministic:
// each step then gets a budget appropriate to what it actually does.
func EnsureImage(ref string) error {
	if _, err := Run(InspectTimeout, "image", "inspect", ref); err == nil {
		return nil // already local; no network dependency at all
	}
	if out, err := Run(PullTimeout, "pull", ref); err != nil {
		return fmt.Errorf("pull %s: %w\n%s", ref, err, out)
	}
	return nil
}

// MustSucceed reports whether a fixture that failed to start should fail the
// test rather than skip it.
//
// Skipping on fixture failure is how a broken fixture hides indefinitely: the
// Service Bus emulator crashed on every CI run for an unknown length of time
// while `go test` still printed `ok` for the package, because every test in it
// skipped. A skip is only honest when the environment genuinely cannot run the
// fixture — no Docker on a developer laptop. If Docker is here and the fixture
// still would not start, that is a failure and must be reported as one.
//
// GOBRIDGE_REQUIRE_FIXTURES=1 forces failure; GOBRIDGE_REQUIRE_FIXTURES=0
// forces the old skip behaviour for a deliberately Docker-less run.
func MustSucceed() bool {
	switch os.Getenv("GOBRIDGE_REQUIRE_FIXTURES") {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	// CI is set by GitHub Actions and every other mainstream CI. There, a
	// fixture that cannot start is always a failure.
	if os.Getenv("CI") != "" {
		return true
	}
	return DockerAvailable()
}

// DockerAvailable reports whether a docker binary is on PATH.
func DockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// RemoveOrphans force-removes every container whose name matches prefix.
// Best-effort sweep for TestMain / ForceStart; errors are ignored.
func RemoveOrphans(prefix string) {
	out, err := Run(InspectTimeout, "ps", "-aq", "--filter", "name="+prefix)
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		_, _ = Run(RemoveTimeout, args...)
	}
}

// IsRunning reports whether the named container is currently running.
func IsRunning(name string) bool {
	out, err := Run(InspectTimeout, "inspect", "--format", "{{.State.Running}}", name)
	return err == nil && len(out) > 0 && out[0] == 't'
}

// WaitHealthy waits until the container reports State.Running, failing fast
// (without burning the timeout) when the container has already exited.
func WaitHealthy(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := Run(InspectTimeout, "inspect",
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
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("container %s did not reach running state within %v", name, timeout)
}

// WaitStopped waits until the container is no longer running (stopped or
// removed) — the deterministic gate between `docker kill/stop` and a restart.
func WaitStopped(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := Run(InspectTimeout, "inspect", "--format", "{{.State.Running}}", name)
		if err != nil || (len(out) > 0 && out[0] == 'f') {
			return nil // stopped, or docker no longer knows it
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("container %s still running after %v", name, timeout)
}

// WaitGone waits until docker no longer knows the container at all — the
// deterministic teardown gate ("disappeared from docker ps -a").
func WaitGone(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := Run(InspectTimeout, "inspect",
			"--format", "{{.State.Running}}", name); err != nil {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("container %s still present after %v", name, timeout)
}

// DrainRemove stops a container gracefully and then removes it, waiting on
// each transition rather than firing and forgetting.
//
// `docker rm -f` SIGKILLs: a broker loses whatever it had not yet flushed, and
// because the call returns before docker has finished, the next start can race
// a half-removed container or a still-bound published port. DrainRemove sends
// SIGTERM, gives the process `timeout` to shut down cleanly, force-removes
// whatever is left, and only returns once docker has genuinely forgotten the
// container — so "stopped" means stopped, the mirror of a real readiness gate.
func DrainRemove(name string, timeout time.Duration) error {
	grace := int(timeout.Seconds())
	if grace < 1 {
		grace = 1
	}
	// `docker stop` blocks for up to the grace period, so allow for it.
	_, _ = Run(RemoveTimeout+timeout, "stop", "-t", strconv.Itoa(grace), name)
	if err := WaitStopped(name, timeout); err != nil {
		return err
	}
	_, _ = Run(RemoveTimeout, "rm", "-f", name)
	return WaitGone(name, timeout)
}

// WaitLogLine waits until the container's log contains substr — a readiness
// gate for services that announce a state transition on stdout/stderr but
// expose no endpoint probeable from the host (e.g. a database initialising
// inside a docker network with no published port).
func WaitLogLine(name, substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := Run(LogsTimeout, "logs", name)
		if strings.Contains(string(out), substr) {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("container %s did not log %q within %v", name, substr, timeout)
}

// WaitTCP waits until 127.0.0.1:port accepts a TCP connection.
func WaitTCP(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("TCP connect to %s failed within %v: %v", addr, timeout, lastErr)
}

// WaitProbe polls probe every interval until it succeeds, failing after
// timeout with the last probe error. The generic protocol-truth readiness
// gate: pass a real API call (ListTables, health GET, AMQP dial, MQTT
// subscribe/publish roundtrip) so "ready" means "processes requests", not
// merely "port open".
func WaitProbe(what string, timeout, interval time.Duration, probe func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = probe(); lastErr == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("%s not ready within %v: %v", what, timeout, lastErr)
}

// Stabilize requires probe to succeed 3 consecutive times, 100ms apart —
// catches containers that accept one connection and then crash-loop.
func Stabilize(probe func() error) error {
	for range stabilizeAttempts {
		if err := probe(); err != nil {
			return err
		}
		time.Sleep(stabilizeInterval)
	}
	return nil
}

// StabilizeTCP is Stabilize with a plain TCP-dial probe.
func StabilizeTCP(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return Stabilize(func() error {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		return conn.Close()
	})
}

// LogFailure dumps the container's recent log tail to stderr for diagnosis.
func LogFailure(name string) {
	out, _ := Run(LogsTimeout, "logs", "--tail", "50", name)
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "--- docker logs %s ---\n%s\n--- end ---\n", name, out)
	}
}

// FreePort returns a TCP port that was free on 127.0.0.1 at call time.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
