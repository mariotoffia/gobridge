package mqttlocal

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// BrokerInstance is an isolated Mosquitto container for a single test.
// Unlike the shared container managed by BrokerURL(), each BrokerInstance
// has its own port, config, and lifecycle. Use this when a test needs
// custom broker settings (e.g., low max_inflight_messages) or needs to
// stop/restart the broker mid-test.
//
// Usage:
//
//	broker := mqttlocal.NewBrokerInstance(t,
//	    mqttlocal.WithMaxInflightMessages(5),
//	    mqttlocal.WithPersistence(true),
//	)
//	defer broker.Stop()
//	url := broker.URL()
//	// ... use url to create MQTT sessions ...
//	broker.Stop()      // simulate broker outage
//	broker.Restart()   // broker comes back
type BrokerInstance struct {
	t        testing.TB
	cfg      config
	port     int
	name     string
	confPath string
	dataDir  string // host-side temp dir for Mosquitto persistence data
	url      string
	stopped  bool
}

// NewBrokerInstance starts a fresh Mosquitto container with the given options.
// The container is automatically removed when the test completes.
func NewBrokerInstance(t testing.TB, opts ...Option) *BrokerInstance {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not found: %v", err)
	}

	c := config{
		image:            "eclipse-mosquitto:latest",
		maxInflightMsgs:  -1,
		maxQueuedMsgs:    -1,
		maxQueuedBytes:   -1,
		messageSizeLimit: -1,
	}
	for _, o := range opts {
		o(&c)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("mqttlocal.NewBrokerInstance: free port: %v", err)
	}

	confContent := buildConfig(c, false)
	confFile, err := os.CreateTemp("", "mqttinstance-*.conf")
	if err != nil {
		t.Fatalf("mqttlocal.NewBrokerInstance: create config: %v", err)
	}
	if _, err := confFile.WriteString(confContent); err != nil {
		_ = confFile.Close()
		_ = os.Remove(confFile.Name())
		t.Fatalf("mqttlocal.NewBrokerInstance: write config: %v", err)
	}
	_ = confFile.Close()

	// When persistence is enabled, create a host-side temp directory for
	// Mosquitto data. This directory is bind-mounted into every container
	// created by start(), so persistence data survives docker rm + docker run.
	var dataDir string
	if c.persistence {
		dir, err := os.MkdirTemp("", "mqttdata-*")
		if err != nil {
			t.Fatalf("mqttlocal.NewBrokerInstance: create data dir: %v", err)
		}
		// Mosquitto in the container runs as uid 1883; ensure the dir is writable.
		if err := os.Chmod(dir, 0o777); err != nil {
			_ = os.RemoveAll(dir)
			t.Fatalf("mqttlocal.NewBrokerInstance: chmod data dir: %v", err)
		}
		dataDir = dir
	}

	b := &BrokerInstance{
		t:        t,
		cfg:      c,
		port:     port,
		name:     fmt.Sprintf("gobridge-mqttinst-%d", port),
		confPath: confFile.Name(),
		dataDir:  dataDir,
		url:      fmt.Sprintf("tcp://127.0.0.1:%d", port),
	}

	b.start()

	t.Cleanup(func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", b.name)
		_ = os.Remove(b.confPath)
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
	})

	return b
}

func (b *BrokerInstance) start() {
	b.t.Helper()

	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", b.name)

	args := []string{
		"run", "-d",
		"--name", b.name,
		"-p", fmt.Sprintf("127.0.0.1:%d:1883", b.port),
		"-v", b.confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}
	if b.dataDir != "" {
		args = append(args, "-v", b.dataDir+":/mosquitto/data")
	}
	if b.cfg.memory != "" {
		args = append(args, "--memory", b.cfg.memory)
	}
	if b.cfg.cpus != "" {
		args = append(args, "--cpus", b.cfg.cpus)
	}
	args = append(args, b.cfg.image)

	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		b.t.Fatalf("mqttlocal.BrokerInstance: docker run: %v\n%s", err, out)
	}

	if err := waitForContainerHealthy(b.name, 15*time.Second); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance: container unhealthy: %v", err)
	}
	if err := waitForTCP(b.port, 30*time.Second); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance: TCP not ready: %v", err)
	}
	if err := stabilize(b.port); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance: stabilize failed: %v", err)
	}

	b.stopped = false
}

// URL returns the MQTT broker URL (tcp://127.0.0.1:<port>).
func (b *BrokerInstance) URL() string { return b.url }

// ContainerName returns the Docker container name.
func (b *BrokerInstance) ContainerName() string { return b.name }

// Stop kills the broker container. Call Restart() to bring it back.
func (b *BrokerInstance) Stop() {
	b.t.Helper()
	if b.stopped {
		return
	}
	out, err := dockerexec.Run(dockerexec.ExecTimeout, "kill", b.name)
	if err != nil {
		b.t.Logf("mqttlocal.BrokerInstance.Stop: docker kill: %v\n%s", err, out)
	}
	b.stopped = true
}

// Restart brings the broker back after a Stop. The same port and config
// are reused so existing MQTT sessions can reconnect.
func (b *BrokerInstance) Restart() {
	b.t.Helper()
	if !b.stopped {
		b.Stop()
	}
	// Remove the dead container so we can reuse the name.
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", b.name)
	b.start()
}

// StopGraceful sends SIGTERM via docker stop, giving Mosquitto time to
// flush persistence data to disk. Unlike Stop (docker kill), the container
// is left intact so RestartGraceful can bring it back with preserved
// session state and queued messages.
func (b *BrokerInstance) StopGraceful() {
	b.t.Helper()
	if b.stopped {
		return
	}
	out, err := dockerexec.Run(dockerexec.RemoveTimeout, "stop", "-t", "5", b.name)
	if err != nil {
		b.t.Logf("mqttlocal.BrokerInstance.StopGraceful: docker stop: %v\n%s", err, out)
	}
	b.stopped = true
}

// RestartGraceful does docker stop + docker start on the SAME container.
// Persistence data, sessions, and queued messages survive because the
// container filesystem is preserved. Use this instead of Restart when the
// test needs broker persistence to work across the restart boundary.
func (b *BrokerInstance) RestartGraceful() {
	b.t.Helper()
	if !b.stopped {
		b.StopGraceful()
	}
	out, err := dockerexec.Run(dockerexec.ExecTimeout, "start", b.name)
	if err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: docker start: %v\n%s", err, out)
	}
	if err := waitForContainerHealthy(b.name, 15*time.Second); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: unhealthy: %v", err)
	}
	if err := waitForTCP(b.port, 30*time.Second); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: TCP not ready: %v", err)
	}
	if err := stabilize(b.port); err != nil {
		logContainerFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: stabilize failed: %v", err)
	}
	b.stopped = false
}

// IsRunning returns true if the broker container is currently running.
func (b *BrokerInstance) IsRunning() bool {
	return !b.stopped && isContainerRunning(b.name)
}
