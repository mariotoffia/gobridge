package servicebus

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// validates ReceiverConfig.validate for queue, topic, subscription, and connection rules.
func TestReceiverConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ReceiverConfig
		wantErr bool
	}{
		{
			name: "valid queue config",
			cfg: ReceiverConfig{
				QueueName:  "my-queue",
				Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://x")},
			},
		},
		{
			name: "valid topic config",
			cfg: ReceiverConfig{
				TopicName:        "my-topic",
				SubscriptionName: "my-sub",
				Connection:       ConnectionConfig{Namespace: "myns.servicebus.windows.net"},
			},
		},
		{
			name:    "missing queue and topic",
			cfg:     ReceiverConfig{Connection: ConnectionConfig{ConnectionString: shared.NewSecret("x")}},
			wantErr: true,
		},
		{
			name: "topic without subscription",
			cfg: ReceiverConfig{
				TopicName:  "my-topic",
				Connection: ConnectionConfig{ConnectionString: shared.NewSecret("x")},
			},
			wantErr: true,
		},
		{
			name:    "missing connection when no client",
			cfg:     ReceiverConfig{QueueName: "q"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// verifies ReceiverConfig.applyDefaults for MaxMessages, MaxWaitTime, AutoExtend, and the 100-message cap.
func TestReceiverConfig_ApplyDefaults(t *testing.T) {
	cfg := ReceiverConfig{}
	cfg.applyDefaults()

	if cfg.MaxMessages != 10 {
		t.Errorf("MaxMessages = %d, want 10", cfg.MaxMessages)
	}
	if cfg.MaxWaitTime != 30*time.Second {
		t.Errorf("MaxWaitTime = %v, want 30s", cfg.MaxWaitTime)
	}
	if cfg.AutoExtend == nil || !*cfg.AutoExtend {
		t.Error("AutoExtend should default to true")
	}

	// Verify cap at 100.
	cfg2 := ReceiverConfig{MaxMessages: 200}
	cfg2.applyDefaults()
	if cfg2.MaxMessages != 100 {
		t.Errorf("MaxMessages = %d, want 100 (capped)", cfg2.MaxMessages)
	}
}

// verifies ReceiverConfig.autoExtendEnabled for nil, true, and false AutoExtend.
func TestReceiverConfig_AutoExtendEnabled(t *testing.T) {
	tests := []struct {
		name string
		val  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ReceiverConfig{AutoExtend: tt.val}
			if got := cfg.autoExtendEnabled(); got != tt.want {
				t.Errorf("autoExtendEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// validates SenderConfig.validate for queue, topic, and connection rules.
func TestSenderConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SenderConfig
		wantErr bool
	}{
		{
			name: "valid queue",
			cfg: SenderConfig{
				QueueName:  "q",
				Connection: ConnectionConfig{ConnectionString: shared.NewSecret("x")},
			},
		},
		{
			name: "valid topic",
			cfg: SenderConfig{
				TopicName:  "t",
				Connection: ConnectionConfig{Namespace: "ns"},
			},
		},
		{
			name:    "missing queue and topic",
			cfg:     SenderConfig{Connection: ConnectionConfig{ConnectionString: shared.NewSecret("x")}},
			wantErr: true,
		},
		{
			name:    "missing connection when no client",
			cfg:     SenderConfig{QueueName: "q"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// verifies SenderConfig.applyDefaults for BatchSize and Timeout.
func TestSenderConfig_ApplyDefaults(t *testing.T) {
	cfg := SenderConfig{}
	cfg.applyDefaults()

	if cfg.BatchSize != 10 {
		t.Errorf("BatchSize = %d, want 10", cfg.BatchSize)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", cfg.Timeout)
	}
}

// --- helpers ---

// D2-FU2 — Config.Validate bounds receiver.lock_duration to the Service Bus
// broker range (5s..5min); 0 means "use the 30s default".
func TestConfig_Validate_LockDurationBounds(t *testing.T) {
	tests := []struct {
		name    string
		lock    time.Duration
		wantErr bool
	}{
		{"zero means default", 0, false},
		{"min 5s ok", 5 * time.Second, false},
		{"max 5m ok", 5 * time.Minute, false},
		{"below min rejected", 2 * time.Second, true},
		{"above max rejected", 6 * time.Minute, true},
		{"negative rejected", -time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Receiver: ReceiverParams{QueueName: "q", LockDuration: tt.lock},
			}
			if err := cfg.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
