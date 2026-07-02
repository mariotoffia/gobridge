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

// verifies ReceiverConfigFromOptions maps option keys into ReceiverConfig and ConnectionConfig.
func TestReceiverConfigFromOptions(t *testing.T) {
	ae := true
	opts := map[string]any{
		"queue_name":           "my-queue",
		"topic_name":           "my-topic",
		"subscription_name":    "my-sub",
		"session_id":           "sess-1",
		"max_messages":         20,
		"max_wait_time":        10 * time.Second,
		"prefetch":             int32(5),
		"receive_mode":         "ReceiveAndDelete",
		"sub_queue":            "deadletter",
		"auto_extend":          ae,
		"connection_string":    "Endpoint=sb://test",
		"namespace":            "myns",
		"use_managed_identity": true,
		"tenant_id":            "tid",
		"client_id":            "cid",
		"client_secret":        "csec",
		"ca_pem":               "PEMDATA",
		"insecure_skip_verify": true,
	}

	cfg := ReceiverConfigFromOptions(opts)

	assertEqual(t, "QueueName", cfg.QueueName, "my-queue")
	assertEqual(t, "TopicName", cfg.TopicName, "my-topic")
	assertEqual(t, "SubscriptionName", cfg.SubscriptionName, "my-sub")
	assertEqual(t, "SessionID", cfg.SessionID, "sess-1")
	assertEqualInt(t, "MaxMessages", cfg.MaxMessages, 20)
	if cfg.MaxWaitTime != 10*time.Second {
		t.Errorf("MaxWaitTime = %v, want 10s", cfg.MaxWaitTime)
	}
	if cfg.Prefetch != 5 {
		t.Errorf("Prefetch = %d, want 5", cfg.Prefetch)
	}
	assertEqual(t, "ReceiveMode", cfg.ReceiveMode, "ReceiveAndDelete")
	assertEqual(t, "SubQueue", cfg.SubQueue, "deadletter")
	if cfg.AutoExtend == nil || !*cfg.AutoExtend {
		t.Error("AutoExtend should be true")
	}
	assertEqual(t, "ConnectionString", cfg.Connection.ConnectionString.Reveal(), "Endpoint=sb://test")
	assertEqual(t, "Namespace", cfg.Connection.Namespace, "myns")
	if !cfg.Connection.UseManagedIdentity {
		t.Error("UseManagedIdentity should be true")
	}
	assertEqual(t, "TenantID", cfg.Connection.TenantID, "tid")
	assertEqual(t, "ClientID", cfg.Connection.ClientID, "cid")
	assertEqual(t, "ClientSecret", cfg.Connection.ClientSecret.Reveal(), "csec")
	assertEqual(t, "CaPEM", cfg.Connection.CaPEM.Reveal(), "PEMDATA")
	if !cfg.Connection.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true")
	}
}

// verifies SenderConfigFromOptions maps option keys into SenderConfig and ConnectionConfig.
func TestSenderConfigFromOptions(t *testing.T) {
	opts := map[string]any{
		"queue_name":         "send-queue",
		"topic_name":         "send-topic",
		"default_session_id": "dsess",
		"batch_size":         25,
		"timeout":            5 * time.Second,
		"connection_string":  "Endpoint=sb://send",
		"namespace":          "sendns",
	}

	cfg := SenderConfigFromOptions(opts)

	assertEqual(t, "QueueName", cfg.QueueName, "send-queue")
	assertEqual(t, "TopicName", cfg.TopicName, "send-topic")
	assertEqual(t, "DefaultSessionID", cfg.DefaultSessionID, "dsess")
	assertEqualInt(t, "BatchSize", cfg.BatchSize, 25)
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Timeout)
	}
	assertEqual(t, "ConnectionString", cfg.Connection.ConnectionString.Reveal(), "Endpoint=sb://send")
	assertEqual(t, "Namespace", cfg.Connection.Namespace, "sendns")
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

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func assertEqualInt(t *testing.T, field string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}
