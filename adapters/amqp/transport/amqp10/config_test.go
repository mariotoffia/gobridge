// Validates configuration types, defaults, validation, and map parsing.
package amqp10

import (
	"testing"
	"time"
)

func TestReceiverConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ReceiverConfig
		wantErr bool
	}{
		{
			name:    "validates that empty address is rejected",
			cfg:     ReceiverConfig{},
			wantErr: true,
		},
		{
			name:    "validates that populated address passes",
			cfg:     ReceiverConfig{Address: "test/queue"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReceiverConfig_ApplyDefaults(t *testing.T) {
	cfg := ReceiverConfig{}
	cfg.applyDefaults()

	if cfg.LinkCredit != 10 {
		t.Fatalf("LinkCredit = %d, want 10", cfg.LinkCredit)
	}
	if cfg.Metrics == nil {
		t.Fatal("Metrics should be non-nil after applyDefaults")
	}
}

func TestReceiverConfig_ApplyDefaults_PreservesExisting(t *testing.T) {
	cfg := ReceiverConfig{LinkCredit: 50}
	cfg.applyDefaults()

	if cfg.LinkCredit != 50 {
		t.Fatalf("LinkCredit = %d, want 50 (should preserve existing)", cfg.LinkCredit)
	}
}

func TestReceiverConfigFromOptions(t *testing.T) {
	m := map[string]any{
		"address":         "queue/inbound",
		"link_credit":     uint32(25),
		"durability_mode": int(2),
	}

	cfg := ReceiverConfigFromOptions(m)

	if cfg.Address != "queue/inbound" {
		t.Fatalf("Address = %q, want %q", cfg.Address, "queue/inbound")
	}
	if cfg.LinkCredit != 25 {
		t.Fatalf("LinkCredit = %d, want 25", cfg.LinkCredit)
	}
	if cfg.DurabilityMode != 2 {
		t.Fatalf("DurabilityMode = %d, want 2", cfg.DurabilityMode)
	}
}

func TestSenderConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SenderConfig
		wantErr bool
	}{
		{
			name:    "validates that empty address is rejected",
			cfg:     SenderConfig{},
			wantErr: true,
		},
		{
			name:    "validates that populated address passes",
			cfg:     SenderConfig{Address: "topic/outbound"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSenderConfig_ApplyDefaults(t *testing.T) {
	cfg := SenderConfig{}
	cfg.applyDefaults()

	if cfg.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", cfg.Timeout)
	}
	if cfg.Metrics == nil {
		t.Fatal("Metrics should be non-nil after applyDefaults")
	}
}

func TestSenderConfig_ApplyDefaults_PreservesExisting(t *testing.T) {
	cfg := SenderConfig{Timeout: 5 * time.Second}
	cfg.applyDefaults()

	if cfg.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %v, want 5s (should preserve existing)", cfg.Timeout)
	}
}

func TestSenderConfigFromOptions(t *testing.T) {
	m := map[string]any{
		"address":         "topic/outbound",
		"timeout":         "10s",
		"durability_mode": int64(1),
	}

	cfg := SenderConfigFromOptions(m)

	if cfg.Address != "topic/outbound" {
		t.Fatalf("Address = %q, want %q", cfg.Address, "topic/outbound")
	}
	if cfg.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %v, want 10s", cfg.Timeout)
	}
	if cfg.DurabilityMode != 1 {
		t.Fatalf("DurabilityMode = %d, want 1", cfg.DurabilityMode)
	}
}

func TestSessionOptionsFromMap(t *testing.T) {
	tests := []struct {
		name    string
		m       map[string]any
		wantErr bool
		check   func(t *testing.T, opts SessionOptions)
	}{
		{
			name:    "validates that missing address returns error",
			m:       map[string]any{},
			wantErr: true,
		},
		{
			name: "verifies all fields parsed correctly",
			m: map[string]any{
				"address":         "amqp://localhost:5672",
				"connect_timeout": "5s",
				"reconnect_delay": "2s",
				"idle_timeout":    time.Duration(3 * time.Minute),
				"max_frame_size":  int(32768),
				"username":        "user1",
				"password":        "pass1",
				"container_id":    "my-container",
			},
			check: func(t *testing.T, opts SessionOptions) {
				t.Helper()
				if opts.Address != "amqp://localhost:5672" {
					t.Fatalf("Address = %q", opts.Address)
				}
				if opts.ConnectTimeout != 5*time.Second {
					t.Fatalf("ConnectTimeout = %v", opts.ConnectTimeout)
				}
				if opts.ReconnectDelay != 2*time.Second {
					t.Fatalf("ReconnectDelay = %v", opts.ReconnectDelay)
				}
				if opts.IdleTimeout != 3*time.Minute {
					t.Fatalf("IdleTimeout = %v", opts.IdleTimeout)
				}
				if opts.MaxFrameSize != 32768 {
					t.Fatalf("MaxFrameSize = %d", opts.MaxFrameSize)
				}
				if opts.Username != "user1" {
					t.Fatalf("Username = %q", opts.Username)
				}
				if opts.Password != "pass1" {
					t.Fatalf("Password = %q", opts.Password)
				}
				if opts.ContainerID != "my-container" {
					t.Fatalf("ContainerID = %q", opts.ContainerID)
				}
			},
		},
		{
			name: "verifies TLS sub-map is parsed",
			m: map[string]any{
				"address": "amqps://broker:5671",
				"tls": map[string]any{
					"enable":               true,
					"ca_cert_file":         "/certs/ca.pem",
					"cert_file":            "/certs/client.pem",
					"key_file":             "/certs/client-key.pem",
					"insecure_skip_verify": true,
				},
			},
			check: func(t *testing.T, opts SessionOptions) {
				t.Helper()
				if opts.TLS == nil {
					t.Fatal("TLS should be non-nil")
				}
				if !opts.TLS.Enable {
					t.Fatal("TLS.Enable should be true")
				}
				if opts.TLS.CACertFile != "/certs/ca.pem" {
					t.Fatalf("TLS.CACertFile = %q", opts.TLS.CACertFile)
				}
				if opts.TLS.CertFile != "/certs/client.pem" {
					t.Fatalf("TLS.CertFile = %q", opts.TLS.CertFile)
				}
				if opts.TLS.KeyFile != "/certs/client-key.pem" {
					t.Fatalf("TLS.KeyFile = %q", opts.TLS.KeyFile)
				}
				if !opts.TLS.InsecureSkipVerify {
					t.Fatal("TLS.InsecureSkipVerify should be true")
				}
			},
		},
		{
			name: "verifies defaults applied when optional fields omitted",
			m: map[string]any{
				"address": "amqp://localhost:5672",
			},
			check: func(t *testing.T, opts SessionOptions) {
				t.Helper()
				if opts.ConnectTimeout != 30*time.Second {
					t.Fatalf("ConnectTimeout = %v, want default 30s", opts.ConnectTimeout)
				}
				if opts.ReconnectDelay != 1*time.Second {
					t.Fatalf("ReconnectDelay = %v, want default 1s", opts.ReconnectDelay)
				}
				if opts.IdleTimeout != 2*time.Minute {
					t.Fatalf("IdleTimeout = %v, want default 2m", opts.IdleTimeout)
				}
				if opts.MaxFrameSize != 65536 {
					t.Fatalf("MaxFrameSize = %d, want default 65536", opts.MaxFrameSize)
				}
			},
		},
		{
			name: "verifies duration from float64 seconds",
			m: map[string]any{
				"address":         "amqp://localhost:5672",
				"connect_timeout": float64(15),
			},
			check: func(t *testing.T, opts SessionOptions) {
				t.Helper()
				if opts.ConnectTimeout != 15*time.Second {
					t.Fatalf("ConnectTimeout = %v, want 15s from float64", opts.ConnectTimeout)
				}
			},
		},
		{
			name: "verifies duration from int seconds",
			m: map[string]any{
				"address":         "amqp://localhost:5672",
				"connect_timeout": int(10),
			},
			check: func(t *testing.T, opts SessionOptions) {
				t.Helper()
				if opts.ConnectTimeout != 10*time.Second {
					t.Fatalf("ConnectTimeout = %v, want 10s from int", opts.ConnectTimeout)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := SessionOptionsFromMap(tt.m)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SessionOptionsFromMap() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, opts)
			}
		})
	}
}

func TestDefaultSessionOptions(t *testing.T) {
	opts := DefaultSessionOptions()

	if opts.ConnectTimeout != 30*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if opts.ReconnectDelay != 1*time.Second {
		t.Fatalf("ReconnectDelay = %v, want 1s", opts.ReconnectDelay)
	}
	if opts.IdleTimeout != 2*time.Minute {
		t.Fatalf("IdleTimeout = %v, want 2m", opts.IdleTimeout)
	}
	if opts.MaxFrameSize != 65536 {
		t.Fatalf("MaxFrameSize = %d, want 65536", opts.MaxFrameSize)
	}
}

func TestDefaultSenderOptions(t *testing.T) {
	opts := DefaultSenderOptions()

	if opts.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", opts.Timeout)
	}
}

func TestSessionOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    SessionOptions
		wantErr bool
	}{
		{
			name:    "validates that empty address is rejected",
			opts:    SessionOptions{},
			wantErr: true,
		},
		{
			name:    "validates that populated address passes",
			opts:    SessionOptions{Address: "amqp://localhost"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSessionOptions_ApplyDefaults(t *testing.T) {
	opts := SessionOptions{}
	opts.applyDefaults()

	if opts.ConnectTimeout != 30*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if opts.ReconnectDelay != 1*time.Second {
		t.Fatalf("ReconnectDelay = %v, want 1s", opts.ReconnectDelay)
	}
	if opts.IdleTimeout != 2*time.Minute {
		t.Fatalf("IdleTimeout = %v, want 2m", opts.IdleTimeout)
	}
	if opts.MaxFrameSize != 65536 {
		t.Fatalf("MaxFrameSize = %d, want 65536", opts.MaxFrameSize)
	}
}
