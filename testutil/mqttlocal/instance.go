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

	// Secure-fixture state. secureDir holds the generated password file and
	// TLS material bind-mounted into the container; the URLs below are empty
	// unless the matching listener was configured.
	secureDir string
	material  *Material
	tlsURL    string
	wsURL     string
	tlsPort   int
	wsPort    int
	wssPort   int
	wssURL    string
}

// NewBrokerInstance starts a fresh Mosquitto container with the given options.
// The container is automatically removed when the test completes.
func NewBrokerInstance(t testing.TB, opts ...Option) *BrokerInstance {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not found: %v", err)
	}

	c := config{
		image:            defaultImage,
		maxInflightMsgs:  -1,
		maxQueuedMsgs:    -1,
		maxQueuedBytes:   -1,
		messageSizeLimit: -1,
	}
	for _, o := range opts {
		o(&c)
	}

	port, err := dockerexec.FreePort()
	if err != nil {
		t.Fatalf("mqttlocal.NewBrokerInstance: free port: %v", err)
	}

	confContent := buildConfig(c)
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
	// os.CreateTemp makes the file 0600. The data dir below is already chmod'ed
	// for the container's uid; the config file needs the same treatment or
	// Mosquitto cannot read it once it drops privileges. Nothing secret here.
	if err := os.Chmod(confFile.Name(), 0o644); err != nil {
		_ = os.Remove(confFile.Name())
		t.Fatalf("mqttlocal.NewBrokerInstance: chmod config: %v", err)
	}

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

	if c.needsSecureMaterial() {
		secureDir, material, materialErr := writeSecureMaterial(c)
		if materialErr != nil {
			t.Fatalf("mqttlocal.NewBrokerInstance: %v", materialErr)
		}
		b.secureDir = secureDir
		b.material = material
	}
	// Each extra listener gets its own host port so the endpoints a test uses
	// are independent: killing one does not free another.
	if c.tls {
		b.tlsPort = freePortOrFatal(t)
		b.tlsURL = fmt.Sprintf("ssl://127.0.0.1:%d", b.tlsPort)
	}
	if c.webSocket {
		b.wsPort = freePortOrFatal(t)
		b.wsURL = fmt.Sprintf("ws://127.0.0.1:%d", b.wsPort)
		if c.tls {
			b.wssPort = freePortOrFatal(t)
			b.wssURL = fmt.Sprintf("wss://127.0.0.1:%d", b.wssPort)
		}
	}

	// Registered BEFORE start: a container that fails its health or readiness
	// gate fatals inside start(), and without this the container, the config
	// file and the generated material would all outlive the test.
	t.Cleanup(func() {
		// Drain rather than SIGKILL: Mosquitto flushes its persistence file on
		// SIGTERM, and waiting for the container to disappear stops a
		// same-named restart from racing this teardown.
		_ = dockerexec.DrainRemove(b.name, dockerexec.RemoveTimeout)
		_ = os.Remove(b.confPath)
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		if b.secureDir != "" {
			_ = os.RemoveAll(b.secureDir)
		}
	})

	b.start()

	return b
}

func (b *BrokerInstance) start() {
	b.t.Helper()

	// Reclaim the name and wait until docker has genuinely forgotten it, so
	// `docker run` cannot collide with a still-terminating container.
	_ = dockerexec.DrainRemove(b.name, dockerexec.RemoveTimeout)

	if err := dockerexec.EnsureImage(b.cfg.image); err != nil {
		b.t.Fatalf("mqttlocal.BrokerInstance: %v", err)
	}

	args := []string{
		"run", "-d",
		"--name", b.name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", b.port, plainPort),
		"-v", b.confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}
	if b.dataDir != "" {
		args = append(args, "-v", b.dataDir+":/mosquitto/data")
	}
	if b.secureDir != "" {
		args = append(args, "-v", b.secureDir+":"+secureMountPath+":ro")
	}
	// Ordered pairs, not a map: an unconfigured listener has host port 0, and
	// two of those would collapse into one map entry while the published order
	// varied run to run.
	for _, listener := range [][2]int{
		{b.tlsPort, tlsPort}, {b.wsPort, wsPort}, {b.wssPort, wssPort},
	} {
		if listener[0] > 0 {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", listener[0], listener[1]))
		}
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

	if err := dockerexec.WaitHealthy(b.name, 15*time.Second); err != nil {
		dockerexec.LogFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance: container unhealthy: %v", err)
	}
	if err := waitBrokerReady(b.port, 30*time.Second, b.cfg.username, b.cfg.password); err != nil {
		dockerexec.LogFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance: broker not ready: %v", err)
	}

	b.stopped = false
}

// URL returns the plaintext MQTT broker URL (tcp://127.0.0.1:<port>).
func (b *BrokerInstance) URL() string { return b.url }

// TLSURL returns the TLS MQTT endpoint (ssl://127.0.0.1:<port>), or "" when
// the fixture was not built [WithTLS].
func (b *BrokerInstance) TLSURL() string { return b.tlsURL }

// WebSocketURL returns the plaintext WebSocket endpoint (ws://127.0.0.1:<port>),
// or "" when the fixture was not built [WithWebSocket].
func (b *BrokerInstance) WebSocketURL() string { return b.wsURL }

// SecureWebSocketURL returns the TLS WebSocket endpoint (wss://127.0.0.1:<port>),
// or "" unless the fixture was built with both [WithWebSocket] and [WithTLS].
func (b *BrokerInstance) SecureWebSocketURL() string { return b.wssURL }

// Material returns the TLS material this fixture generated, or nil when it
// serves no TLS listener. See [Material] for what a client validates with.
func (b *BrokerInstance) Material() *Material { return b.material }

// Credentials returns the username and password this fixture requires, or two
// empty strings when it allows anonymous access.
func (b *BrokerInstance) Credentials() (string, string) {
	return b.cfg.username, b.cfg.password
}

func freePortOrFatal(t testing.TB) int {
	t.Helper()
	port, err := dockerexec.FreePort()
	if err != nil {
		t.Fatalf("mqttlocal: free port: %v", err)
	}
	return port
}

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
	// Deterministic post-condition: "stopped" means docker reports the
	// container not running, not merely "the kill signal was sent".
	if err := dockerexec.WaitStopped(b.name, 10*time.Second); err != nil {
		b.t.Fatalf("mqttlocal.BrokerInstance.Stop: %v", err)
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
	// Remove the dead container so we can reuse the name, waiting until it is
	// actually gone before starting a replacement.
	_ = dockerexec.DrainRemove(b.name, dockerexec.RemoveTimeout)
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
	if err := dockerexec.WaitStopped(b.name, 15*time.Second); err != nil {
		b.t.Fatalf("mqttlocal.BrokerInstance.StopGraceful: %v", err)
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
		dockerexec.LogFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: docker start: %v\n%s", err, out)
	}
	if err := dockerexec.WaitHealthy(b.name, 15*time.Second); err != nil {
		dockerexec.LogFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: unhealthy: %v", err)
	}
	if err := waitBrokerReady(b.port, 30*time.Second, b.cfg.username, b.cfg.password); err != nil {
		dockerexec.LogFailure(b.name)
		b.t.Fatalf("mqttlocal.BrokerInstance.RestartGraceful: broker not ready: %v", err)
	}
	b.stopped = false
}

// IsRunning returns true if the broker container is currently running.
func (b *BrokerInstance) IsRunning() bool {
	return !b.stopped && dockerexec.IsRunning(b.name)
}
