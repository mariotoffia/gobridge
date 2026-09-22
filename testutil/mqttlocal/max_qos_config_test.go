package mqttlocal

import (
	"strings"
	"testing"
)

// Mosquitto scopes max_qos to the listener it follows, so a cap rendered once at
// the end of the file would leave every listener but the last one uncapped. A
// value outside 0..2 stops Mosquitto from starting at all, which surfaces as a
// fixture that never becomes ready, far from the option that caused it. Both are
// pinned at render time, without Docker.
//
// Category: unit (TESTS.md §1).

func TestBuildConfig_MaxQoSCapsEveryListener(t *testing.T) {
	c := defaultConfig()
	c.webSocket, c.tls = true, true
	WithMaxQoS(0)(&c)

	rendered, err := buildConfig(c)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	blocks := strings.Split(rendered, "listener ")[1:]
	if len(blocks) != 4 {
		t.Fatalf("rendered %d listeners, want plain, WebSocket, TLS and secure WebSocket:\n%s", len(blocks), rendered)
	}
	for _, block := range blocks {
		if !strings.Contains(block, "\nmax_qos 0\n") {
			t.Fatalf("listener %s is not capped:\n%s", strings.SplitN(block, "\n", 2)[0], rendered)
		}
	}
}

func TestBuildConfig_MaxQoSMinusOneRestoresTheBrokerDefault(t *testing.T) {
	c := defaultConfig()
	WithMaxQoS(0)(&c)
	WithMaxQoS(-1)(&c)

	rendered, err := buildConfig(c)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if strings.Contains(rendered, "max_qos") {
		t.Fatalf("WithMaxQoS(-1) must render no cap, so Mosquitto's default of 2 applies:\n%s", rendered)
	}
}

func TestBuildConfig_MaxQoSOutsideZeroToTwoIsRejected(t *testing.T) {
	for _, qos := range []int{-2, 3} {
		c := defaultConfig()
		WithMaxQoS(qos)(&c)
		if rendered, err := buildConfig(c); err == nil {
			t.Errorf("WithMaxQoS(%d) rendered a config Mosquitto would refuse to start with:\n%s", qos, rendered)
		}
	}
}
