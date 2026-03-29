package mqttlocal

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

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

func buildConfig(c config, hasWS bool) string {
	s := "listener 1883 0.0.0.0\nprotocol mqtt\n\n"
	if hasWS {
		s += "listener 9001 0.0.0.0\nprotocol websockets\n\n"
	}
	s += "allow_anonymous true\n\n"
	if c.persistence {
		s += "persistence true\npersistence_location /mosquitto/data/\n"
	} else {
		s += "persistence false\n"
	}
	if c.maxInflightMsgs >= 0 {
		s += fmt.Sprintf("max_inflight_messages %d\n", c.maxInflightMsgs)
	}
	if c.maxQueuedMsgs >= 0 {
		s += fmt.Sprintf("max_queued_messages %d\n", c.maxQueuedMsgs)
	}
	if c.maxQueuedBytes >= 0 {
		s += fmt.Sprintf("max_queued_bytes %d\n", c.maxQueuedBytes)
	}
	if c.messageSizeLimit >= 0 {
		s += fmt.Sprintf("message_size_limit %d\n", c.messageSizeLimit)
	}
	if c.extraConfig != "" {
		s += c.extraConfig
	}
	s += "\nlog_dest stdout\n"
	return s
}

func waitForTCP(port int, timeout time.Duration) error {
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
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("TCP connect to %s failed within %v: %v", addr, timeout, lastErr)
}

func stabilize(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for range 3 {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		_ = conn.Close()
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
