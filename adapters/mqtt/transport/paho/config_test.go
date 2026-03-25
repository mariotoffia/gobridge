package paho

import (
	"testing"
	"time"
)

// verifies SessionOptionsFromMap applies defaults when the input map is nil.
func TestSessionOptionsFromMap_Defaults(t *testing.T) {
	opts := SessionOptionsFromMap(nil)
	if opts.KeepAlive != 30 {
		t.Errorf("KeepAlive = %d, want 30", opts.KeepAlive)
	}
	if opts.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if !opts.CleanStart {
		t.Error("CleanStart should default to true")
	}
}

// verifies SessionOptionsFromMap reads broker_urls and client_id.
func TestSessionOptionsFromMap_BrokerURLs(t *testing.T) {
	m := map[string]any{
		"broker_urls": []string{"tcp://a:1883", "tcp://b:1883"},
		"client_id":   "test-client",
	}
	opts := SessionOptionsFromMap(m)

	if len(opts.BrokerURLs) != 2 {
		t.Fatalf("BrokerURLs len = %d, want 2", len(opts.BrokerURLs))
	}
	if opts.ClientID != "test-client" {
		t.Errorf("ClientID = %q, want %q", opts.ClientID, "test-client")
	}
}

// verifies SessionOptionsFromMap accepts a single broker_url string.
func TestSessionOptionsFromMap_SingleBrokerURL(t *testing.T) {
	m := map[string]any{
		"broker_url": "tcp://single:1883",
	}
	opts := SessionOptionsFromMap(m)

	if len(opts.BrokerURLs) != 1 || opts.BrokerURLs[0] != "tcp://single:1883" {
		t.Errorf("BrokerURLs = %v, want [tcp://single:1883]", opts.BrokerURLs)
	}
}

// verifies SessionOptionsFromMap maps username and password.
func TestSessionOptionsFromMap_Auth(t *testing.T) {
	m := map[string]any{
		"username": "user",
		"password": "pass",
	}
	opts := SessionOptionsFromMap(m)

	if opts.Username != "user" {
		t.Errorf("Username = %q, want %q", opts.Username, "user")
	}
	if opts.Password != "pass" {
		t.Errorf("Password = %q, want %q", opts.Password, "pass")
	}
}

// verifies SessionOptionsFromMap builds TLS options from a nested tls map.
func TestSessionOptionsFromMap_TLSFromMap(t *testing.T) {
	m := map[string]any{
		"tls": map[string]any{
			"enable":               true,
			"insecure_skip_verify": true,
		},
	}
	opts := SessionOptionsFromMap(m)

	if opts.TLS == nil {
		t.Fatal("TLS should be set")
	}
	if !opts.TLS.Enable {
		t.Error("TLS.Enable should be true")
	}
	if !opts.TLS.InsecureSkipVerify {
		t.Error("TLS.InsecureSkipVerify should be true")
	}
}

// verifies SessionOptionsFromMap maps session_expiry_interval and clean_start.
func TestSessionOptionsFromMap_SessionExpiry(t *testing.T) {
	m := map[string]any{
		"session_expiry_interval": 3600,
		"clean_start":            false,
	}
	opts := SessionOptionsFromMap(m)

	if opts.SessionExpiryInterval != 3600 {
		t.Errorf("SessionExpiryInterval = %d, want 3600", opts.SessionExpiryInterval)
	}
	if opts.CleanStart {
		t.Error("CleanStart should be false")
	}
}

// verifies SenderOptionsFromMap applies defaults when the input map is nil.
func TestSenderOptionsFromMap_Defaults(t *testing.T) {
	opts := SenderOptionsFromMap(nil)
	if opts.QoS != 1 {
		t.Errorf("QoS = %d, want 1", opts.QoS)
	}
	if opts.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", opts.Timeout)
	}
}

// verifies SenderOptionsFromMap maps default_topic, qos, retain, and timeout.
func TestSenderOptionsFromMap_AllFields(t *testing.T) {
	m := map[string]any{
		"default_topic": "my/topic",
		"qos":           2,
		"retain":        true,
		"timeout":       10 * time.Second,
	}
	opts := SenderOptionsFromMap(m)

	if opts.DefaultTopic != "my/topic" {
		t.Errorf("DefaultTopic = %q, want %q", opts.DefaultTopic, "my/topic")
	}
	if opts.QoS != 2 {
		t.Errorf("QoS = %d, want 2", opts.QoS)
	}
	if !opts.Retain {
		t.Error("Retain should be true")
	}
	if opts.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", opts.Timeout)
	}
}

// verifies SenderOptionsFromMap falls back to default QoS when qos is out of range.
func TestSenderOptionsFromMap_InvalidQoS(t *testing.T) {
	m := map[string]any{"qos": 5}
	opts := SenderOptionsFromMap(m)
	if opts.QoS != 1 {
		t.Errorf("QoS = %d, want 1 (default for invalid)", opts.QoS)
	}
}

// TestReceiverOptionsFromMap_Defaults validates that ReceiverOptionsFromMap
// returns a valid ReceiverOptions from an empty map.
func TestReceiverOptionsFromMap_Defaults(t *testing.T) {
	opts := ReceiverOptionsFromMap(nil)
	_ = opts
}

// TestReceiverOptionsFromMap_NonNilMap validates ReceiverOptionsFromMap
// with a populated options map (currently no receiver-specific options).
func TestReceiverOptionsFromMap_NonNilMap(t *testing.T) {
	opts := ReceiverOptionsFromMap(map[string]any{
		"some_unknown_key": "ignored",
	})
	_ = opts
}
