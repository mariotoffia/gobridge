package mqttlocal

import "fmt"

// buildConfig renders the mosquitto.conf for the given options. Listener ports
// are the container-internal ones from secure.go; the host side maps them.
//
// Container lifecycle plumbing (orphan sweep, healthy/TCP/stabilize gates,
// log capture, free ports) lives in testutil/dockerexec — shared by every
// testutil/*local launcher.
func buildConfig(c config) string {
	s := fmt.Sprintf("listener %d 0.0.0.0\nprotocol mqtt\n\n", plainPort)
	if c.webSocket {
		s += fmt.Sprintf("listener %d 0.0.0.0\nprotocol websockets\n\n", wsPort)
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
	return s
}
