package amqp091

import (
	"testing"
	"time"
)

// verifies SessionOptions.validate rejects empty broker URL.
func TestSessionOptions_Validate_RequiresBrokerURL(t *testing.T) {
	opts := SessionOptions{}
	if err := opts.validate(); err == nil {
		t.Fatal("expected error for empty broker_url")
	}
}

// verifies SessionOptions.validate accepts a valid broker URL.
func TestSessionOptions_Validate_Valid(t *testing.T) {
	opts := SessionOptions{BrokerURL: "amqp://localhost:5672/"}
	if err := opts.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// verifies applyDefaults fills zero-value durations with recommended defaults.
func TestSessionOptions_ApplyDefaults(t *testing.T) {
	opts := SessionOptions{BrokerURL: "amqp://localhost/"}
	opts.applyDefaults()

	if opts.Heartbeat != 10*time.Second {
		t.Errorf("Heartbeat = %v, want 10s", opts.Heartbeat)
	}
	if opts.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if opts.ReconnectDelay != 1*time.Second {
		t.Errorf("ReconnectDelay = %v, want 1s", opts.ReconnectDelay)
	}
}

// verifies applyDefaults preserves non-zero values.
func TestSessionOptions_ApplyDefaults_PreservesCustom(t *testing.T) {
	opts := SessionOptions{
		BrokerURL:      "amqp://localhost/",
		Heartbeat:      5 * time.Second,
		ConnectTimeout: 15 * time.Second,
		ReconnectDelay: 3 * time.Second,
	}
	opts.applyDefaults()

	if opts.Heartbeat != 5*time.Second {
		t.Errorf("Heartbeat = %v, want 5s", opts.Heartbeat)
	}
	if opts.ConnectTimeout != 15*time.Second {
		t.Errorf("ConnectTimeout = %v, want 15s", opts.ConnectTimeout)
	}
	if opts.ReconnectDelay != 3*time.Second {
		t.Errorf("ReconnectDelay = %v, want 3s", opts.ReconnectDelay)
	}
}

// verifies SessionOptionsFromMap round-trips all fields from a generic map.
func TestSessionOptionsFromMap(t *testing.T) {
	m := map[string]any{
		"broker_url":      "amqp://rabbit:5672/",
		"username":        "admin",
		"password":        "secret",
		"vhost":           "/prod",
		"heartbeat":       20 * time.Second,
		"connect_timeout": 45 * time.Second,
		"reconnect_delay": 5 * time.Second,
	}

	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.BrokerURL != "amqp://rabbit:5672/" {
		t.Errorf("BrokerURL = %q", opts.BrokerURL)
	}
	if opts.Username != "admin" {
		t.Errorf("Username = %q", opts.Username)
	}
	if opts.Password.Reveal() != "secret" {
		t.Errorf("Password = %q", opts.Password.Reveal())
	}
	if opts.Vhost != "/prod" {
		t.Errorf("Vhost = %q", opts.Vhost)
	}
	if opts.Heartbeat != 20*time.Second {
		t.Errorf("Heartbeat = %v", opts.Heartbeat)
	}
	if opts.ConnectTimeout != 45*time.Second {
		t.Errorf("ConnectTimeout = %v", opts.ConnectTimeout)
	}
	if opts.ReconnectDelay != 5*time.Second {
		t.Errorf("ReconnectDelay = %v", opts.ReconnectDelay)
	}
}

// verifies SessionOptionsFromMap returns validation error when the map is nil
// (broker_url is required).
func TestSessionOptionsFromMap_Nil(t *testing.T) {
	_, err := SessionOptionsFromMap(nil)
	if err == nil {
		t.Fatal("expected validation error for nil map (missing broker_url)")
	}
}

// verifies SessionOptionsFromMap parses TLS config from a nested map.
func TestSessionOptionsFromMap_TLSMap(t *testing.T) {
	m := map[string]any{
		"broker_url": "amqps://rabbit:5671/",
		"tls": map[string]any{
			"enable":               true,
			"ca_cert_file":         "/certs/ca.pem",
			"cert_file":            "/certs/client.pem",
			"key_file":             "/certs/client.key",
			"insecure_skip_verify": true,
		},
	}

	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.TLS == nil {
		t.Fatal("TLS config is nil")
	}
	if !opts.TLS.Enable {
		t.Error("TLS.Enable = false")
	}
	if opts.TLS.CACertFile != "/certs/ca.pem" {
		t.Errorf("TLS.CACertFile = %q", opts.TLS.CACertFile)
	}
	if opts.TLS.CertFile != "/certs/client.pem" {
		t.Errorf("TLS.CertFile = %q", opts.TLS.CertFile)
	}
	if opts.TLS.KeyFile != "/certs/client.key" {
		t.Errorf("TLS.KeyFile = %q", opts.TLS.KeyFile)
	}
	if !opts.TLS.InsecureSkipVerify {
		t.Error("TLS.InsecureSkipVerify = false")
	}
}

// verifies SessionOptionsFromMap accepts a *TLSConfig directly.
func TestSessionOptionsFromMap_TLSStruct(t *testing.T) {
	cfg := &TLSConfig{Enable: true, CACertFile: "/ca.pem"}
	m := map[string]any{
		"broker_url": "amqps://rabbit/",
		"tls":        cfg,
	}

	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.TLS != cfg {
		t.Error("TLS config pointer mismatch")
	}
}

// verifies SenderConfig.validate accepts any config (no required fields).
func TestSenderConfig_Validate(t *testing.T) {
	cfg := SenderConfig{}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// verifies SenderConfig.applyDefaults sets a 30s timeout when zero.
func TestSenderConfig_ApplyDefaults(t *testing.T) {
	cfg := SenderConfig{}
	cfg.applyDefaults()

	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", cfg.Timeout)
	}
}

// verifies SenderConfig.applyDefaults preserves a custom timeout.
func TestSenderConfig_ApplyDefaults_PreservesCustom(t *testing.T) {
	cfg := SenderConfig{Timeout: 10 * time.Second}
	cfg.applyDefaults()

	if cfg.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", cfg.Timeout)
	}
}

// verifies SenderConfigFromOptions round-trips all fields from a generic map.
func TestSenderConfigFromOptions(t *testing.T) {
	m := map[string]any{
		"exchange":    "orders",
		"routing_key": "order.created",
		"mandatory":   true,
		"immediate":   true,
		"timeout":     15 * time.Second,
	}

	cfg := SenderConfigFromOptions(m)

	if cfg.Exchange != "orders" {
		t.Errorf("Exchange = %q", cfg.Exchange)
	}
	if cfg.RoutingKey != "order.created" {
		t.Errorf("RoutingKey = %q", cfg.RoutingKey)
	}
	if !cfg.Mandatory {
		t.Error("Mandatory = false")
	}
	if !cfg.Immediate {
		t.Error("Immediate = false")
	}
	if cfg.Timeout != 15*time.Second {
		t.Errorf("Timeout = %v", cfg.Timeout)
	}
}

// verifies SenderConfigFromOptions returns defaults for a nil map.
func TestSenderConfigFromOptions_Nil(t *testing.T) {
	cfg := SenderConfigFromOptions(nil)
	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", cfg.Timeout)
	}
}

// verifies ReceiverConfigFromOptions round-trips all fields from a generic map.
func TestReceiverConfigFromOptions(t *testing.T) {
	m := map[string]any{
		"queue_name":     "events",
		"consumer_tag":   "consumer-1",
		"auto_ack":       true,
		"exclusive":      true,
		"prefetch_count": 50,
		"prefetch_size":  1024,
	}

	cfg := ReceiverConfigFromOptions(m)

	if cfg.QueueName != "events" {
		t.Errorf("QueueName = %q", cfg.QueueName)
	}
	if cfg.ConsumerTag != "consumer-1" {
		t.Errorf("ConsumerTag = %q", cfg.ConsumerTag)
	}
	if !cfg.AutoAck {
		t.Error("AutoAck = false")
	}
	if !cfg.Exclusive {
		t.Error("Exclusive = false")
	}
	if cfg.PrefetchCount != 50 {
		t.Errorf("PrefetchCount = %d", cfg.PrefetchCount)
	}
	if cfg.PrefetchSize != 1024 {
		t.Errorf("PrefetchSize = %d", cfg.PrefetchSize)
	}
}

// verifies ReceiverConfigFromOptions returns defaults for a nil map.
func TestReceiverConfigFromOptions_Nil(t *testing.T) {
	cfg := ReceiverConfigFromOptions(nil)
	if cfg.PrefetchCount != 10 {
		t.Errorf("PrefetchCount = %d, want 10", cfg.PrefetchCount)
	}
	if cfg.QueueName != "" {
		t.Errorf("QueueName = %q, want empty", cfg.QueueName)
	}
}

// verifies DefaultSessionOptions returns recommended initial values.
func TestDefaultSessionOptions(t *testing.T) {
	d := DefaultSessionOptions()
	if d.Heartbeat != 10*time.Second {
		t.Errorf("Heartbeat = %v", d.Heartbeat)
	}
	if d.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v", d.ConnectTimeout)
	}
	if d.ReconnectDelay != 1*time.Second {
		t.Errorf("ReconnectDelay = %v", d.ReconnectDelay)
	}
}

// verifies DefaultSenderOptions returns a 30s timeout.
func TestDefaultSenderOptions(t *testing.T) {
	d := DefaultSenderOptions()
	if d.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", d.Timeout)
	}
}

// verifies BuildTLSConfig returns nil for a nil input.
func TestBuildTLSConfig_Nil(t *testing.T) {
	cfg, err := BuildTLSConfig(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil tls.Config")
	}
}

// verifies BuildTLSConfig sets InsecureSkipVerify from the input.
func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{Enable: true, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false")
	}
}

// verifies BuildTLSConfig returns nil when Enable is false.
func TestBuildTLSConfig_EnableFalse(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{Enable: false, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil tls.Config when Enable is false")
	}
}

// verifies BuildTLSConfig returns an error for a non-existent CA cert file.
func TestBuildTLSConfig_BadCACert(t *testing.T) {
	_, err := BuildTLSConfig(&TLSConfig{Enable: true, CACertFile: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("expected error for bad CA cert path")
	}
}

// verifies BuildTLSConfig returns an error for a non-existent client cert pair.
func TestBuildTLSConfig_BadClientCert(t *testing.T) {
	_, err := BuildTLSConfig(&TLSConfig{
		Enable:   true,
		CertFile: "/nonexistent/client.pem",
		KeyFile:  "/nonexistent/client.key",
	})
	if err == nil {
		t.Fatal("expected error for bad client cert path")
	}
}

// verifies SessionOptionsFromMap rejects maps with wrong-typed broker_url.
func TestSessionOptionsFromMap_WrongTypes(t *testing.T) {
	m := map[string]any{
		"broker_url": 42,
		"heartbeat":  "not-a-duration",
	}

	_, err := SessionOptionsFromMap(m)
	if err == nil {
		t.Fatal("expected error: wrong-typed broker_url should fail validation")
	}
}

// verifies SessionOptionsFromMap ignores wrong-typed optional fields gracefully.
func TestSessionOptionsFromMap_WrongOptionalTypes(t *testing.T) {
	m := map[string]any{
		"broker_url": "amqp://localhost/",
		"heartbeat":  "not-a-duration",
	}

	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Heartbeat != 10*time.Second {
		t.Errorf("Heartbeat = %v, want default 10s", opts.Heartbeat)
	}
}
