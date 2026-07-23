package mqttlocal

import "fmt"

// buildConfig renders the mosquitto.conf for the given options.
//
// Container lifecycle plumbing (orphan sweep, healthy/TCP/stabilize gates,
// log capture, free ports) lives in testutil/dockerexec — shared by every
// testutil/*local launcher.
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
