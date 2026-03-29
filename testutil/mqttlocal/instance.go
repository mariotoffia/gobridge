package mqttlocal

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
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
	t         testing.TB
	cfg       config
	port      int
	name      string
	confPath  string
	url       string
	stopped   bool
}

// NewBrokerInstance starts a fresh Mosquitto container with the given options.
// The container is automatically removed when the test completes.
func NewBrokerInstance(t testing.TB, opts ...Option) *BrokerInstance {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not found: %v", err)
	}

	c := config{
		image:           "eclipse-mosquitto:latest",
		maxInflightMsgs: -1,
		maxQueuedMsgs:   -1,
		maxQueuedBytes:  -1,
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

	b := &BrokerInstance{
		t:        t,
		cfg:      c,
		port:     port,
		name:     fmt.Sprintf("gobridge-mqttinst-%d", port),
		confPath: confFile.Name(),
		url:      fmt.Sprintf("tcp://127.0.0.1:%d", port),
	}

	b.start()

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", b.name).Run()
		_ = os.Remove(b.confPath)
	})

	return b
}

func (b *BrokerInstance) start() {
	b.t.Helper()

	_ = exec.Command("docker", "rm", "-f", b.name).Run()

	args := []string{
		"run", "-d",
		"--name", b.name,
		"-p", fmt.Sprintf("127.0.0.1:%d:1883", b.port),
		"-v", b.confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}
	if b.cfg.memory != "" {
		args = append(args, "--memory", b.cfg.memory)
	}
	if b.cfg.cpus != "" {
		args = append(args, "--cpus", b.cfg.cpus)
	}
	args = append(args, b.cfg.image)

	out, err := exec.Command("docker", args...).CombinedOutput()
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
	out, err := exec.Command("docker", "kill", b.name).CombinedOutput()
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
	_ = exec.Command("docker", "rm", "-f", b.name).Run()
	b.start()
}

// IsRunning returns true if the broker container is currently running.
func (b *BrokerInstance) IsRunning() bool {
	return !b.stopped && isContainerRunning(b.name)
}
