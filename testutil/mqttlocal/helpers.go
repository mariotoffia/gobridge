package mqttlocal

import "fmt"

// buildConfig renders the mosquitto.conf for the given options. Listener ports
// are the container-internal ones from secure.go; the host side maps them.
//
// Container lifecycle plumbing (orphan sweep, healthy/TCP/stabilize gates,
// log capture, free ports) lives in testutil/dockerexec — shared by every
// testutil/*local launcher.
func buildConfig(c config) (string, error) {
	// Mosquitto refuses to start on any other value, which would surface as a
	// fixture that never becomes ready instead of as the option at fault.
	if c.maxQoS < -1 || c.maxQoS > 2 {
		return "", fmt.Errorf("mqttlocal: WithMaxQoS(%d): use 0, 1 or 2, or -1 for the broker default", c.maxQoS)
	}
	s := fmt.Sprintf("listener %d 0.0.0.0\nprotocol mqtt\n%s\n", plainPort, listenerLimits(c))
	if c.webSocket {
		s += fmt.Sprintf("listener %d 0.0.0.0\nprotocol websockets\n%s\n", wsPort, listenerLimits(c))
	}
	s += secureListenerLines(c)
	// allow_anonymous is global in Mosquitto: with a password file present
	// every listener, plaintext and TLS alike, demands credentials.
	s += fmt.Sprintf("\nallow_anonymous %t\n\n", c.username == "")
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
	return s, nil
}

// listenerLimits renders the settings Mosquitto scopes to the listener they
// follow, for every listener block to repeat: rendered once at the end of the
// file, like WithExtraConfig, they would bind only the last listener.
func listenerLimits(c config) string {
	if c.maxQoS < 0 {
		return ""
	}
	return fmt.Sprintf("max_qos %d\n", c.maxQoS)
}
