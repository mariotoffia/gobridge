package mqttlocal

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// ---------------------------------------------------------------------------
// Container lifecycle
// ---------------------------------------------------------------------------

func startContainer(c config) (mqttURL, wsURLOut, cName string, cleanup func(), err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", "", "", nil, fmt.Errorf("docker not found: %w", err)
	}

	if c.cleanOrphans {
		dockerexec.RemoveOrphans(containerPrefix)
	}

	mqttPort, err := dockerexec.FreePort()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("find free MQTT port: %w", err)
	}

	var wsHostPort int
	if c.webSocket {
		wsHostPort, err = dockerexec.FreePort()
		if err != nil {
			return "", "", "", nil, fmt.Errorf("find free WebSocket port: %w", err)
		}
	}

	confContent := buildConfig(c)

	// An authenticated or certificate-serving shared fixture needs its
	// material on disk before Mosquitto reads the config that names it.
	var secureDir string
	if c.needsSecureMaterial() {
		dir, _, materialErr := writeSecureMaterial(c)
		if materialErr != nil {
			return "", "", "", nil, materialErr
		}
		secureDir = dir
	}

	// Every failure below has to take the material directory with it, or a
	// fixture that could not start leaves a temp directory behind on every run.
	discardMaterial := func(wrapped error) (string, string, string, func(), error) {
		if secureDir != "" {
			_ = os.RemoveAll(secureDir)
		}
		return "", "", "", nil, wrapped
	}

	confFile, err := os.CreateTemp("", "mqttlocal-*.conf")
	if err != nil {
		return discardMaterial(fmt.Errorf("create temp config: %w", err))
	}
	if _, err := confFile.WriteString(confContent); err != nil {
		_ = confFile.Close()
		_ = os.Remove(confFile.Name())
		return discardMaterial(fmt.Errorf("write config: %w", err))
	}
	_ = confFile.Close()
	confPath := confFile.Name()

	// os.CreateTemp makes the file 0600, owned by the user running the tests.
	// A bind mount preserves that uid and mode on Linux, so a container running
	// as a non-root user cannot read it. Nothing secret here.
	if err := os.Chmod(confPath, 0o644); err != nil {
		_ = os.Remove(confPath)
		return discardMaterial(fmt.Errorf("chmod config readable by container uid: %w", err))
	}

	name := fmt.Sprintf("gobridge-mqtt-%d", mqttPort)

	// Reclaim the name and wait until docker has forgotten it, so the run
	// below cannot collide with a still-terminating container.
	_ = dockerexec.DrainRemove(name, dockerexec.RemoveTimeout)

	if err := dockerexec.EnsureImage(c.image); err != nil {
		_ = os.Remove(confPath)
		return discardMaterial(err)
	}

	args := []string{
		"run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", mqttPort, plainPort),
		"-v", confPath + ":/mosquitto/config/mosquitto.conf:ro",
	}

	if c.webSocket && wsHostPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", wsHostPort, wsPort))
	}
	if secureDir != "" {
		args = append(args, "-v", secureDir+":"+secureMountPath+":ro")
	}
	if c.memory != "" {
		args = append(args, "--memory", c.memory)
	}
	if c.cpus != "" {
		args = append(args, "--cpus", c.cpus)
	}

	args = append(args, c.image)

	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		_ = os.Remove(confPath)
		return discardMaterial(fmt.Errorf("docker run: %w\n%s", err, out))
	}

	cleanup = func() {
		// Drain so Mosquitto flushes persistence on SIGTERM, and so the
		// published port is released before the next fixture claims one.
		_ = dockerexec.DrainRemove(name, dockerexec.RemoveTimeout)
		_ = os.Remove(confPath)
		if secureDir != "" {
			_ = os.RemoveAll(secureDir)
		}
	}

	if err := dockerexec.WaitHealthy(name, 15*time.Second); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto container failed: %w", err)
	}

	// Gate on protocol truth, not on the port. Accepting TCP does NOT imply
	// Mosquitto is operational: with Docker's userland proxy the host port is
	// bound at container creation, so a dial succeeds while the broker is
	// still loading config or restoring persistence — or has already exited.
	// waitBrokerReady requires a real publish/deliver roundtrip, so returning
	// from startContainer means the broker moves messages.
	if err := waitBrokerReady(mqttPort, 30*time.Second, c.username, c.password); err != nil {
		dockerexec.LogFailure(name)
		cleanup()
		return "", "", "", nil, fmt.Errorf("mosquitto not ready: %w", err)
	}

	mqttURL = fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort)
	if c.webSocket && wsHostPort > 0 {
		wsURLOut = fmt.Sprintf("ws://127.0.0.1:%d", wsHostPort)
	}

	return mqttURL, wsURLOut, name, cleanup, nil
}

// buildConfig is in helpers.go; container lifecycle gates (WaitHealthy,
// WaitTCP, StabilizeTCP, RemoveOrphans, LogFailure, FreePort) come from
// testutil/dockerexec.
