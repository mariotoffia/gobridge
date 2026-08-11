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
			// entityNameFor would silently select the queue and
			// ignore the topic/subscription — reject the ambiguous config.
			name: "both queue and topic+subscription rejected",
			cfg: ReceiverConfig{
				QueueName:        "q",
				TopicName:        "my-topic",
				SubscriptionName: "my-sub",
				Connection:       ConnectionConfig{ConnectionString: shared.NewSecret("x")},
			},
			wantErr: true,
		},
		{
			// A stray topic alongside a queue (even without a subscription)
			// is still ambiguous and must be rejected.
			name: "queue with stray topic rejected",
			cfg: ReceiverConfig{
				QueueName:  "q",
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
			// entityName would silently select the queue and ignore
			// the topic — reject the ambiguous config.
			name: "both queue and topic rejected",
			cfg: SenderConfig{
				QueueName:  "q",
				TopicName:  "t",
				Connection: ConnectionConfig{ConnectionString: shared.NewSecret("x")},
			},
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

// Config.Validate bounds receiver.lock_duration to the Service Bus
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

// TestValidateReceiverEntity_RejectsBothQueueAndTopic pins on the
// plugin build boundary (factory.NewReceiver → Config.ValidateReceiverEntity):
// a config that names BOTH a queue and a topic/subscription must fail fast
// rather than silently consuming from the queue.
//
// Mutation: drop the validateReceiverEntityExclusive call in
// ValidateReceiverEntity. Then the both-set cases return nil and the
// wantErr assertions FAIL.
func TestValidateReceiverEntity_RejectsBothQueueAndTopic(t *testing.T) {
	tests := []struct {
		name    string
		params  ReceiverParams
		wantErr bool
	}{
		{"queue only", ReceiverParams{QueueName: "q"}, false},
		{"topic+subscription only", ReceiverParams{TopicName: "t", SubscriptionName: "s"}, false},
		{"neither rejected", ReceiverParams{}, true},
		{"both queue and topic+subscription rejected", ReceiverParams{QueueName: "q", TopicName: "t", SubscriptionName: "s"}, true},
		{"queue with stray topic rejected", ReceiverParams{QueueName: "q", TopicName: "t"}, true},
		{"queue with stray subscription rejected", ReceiverParams{QueueName: "q", SubscriptionName: "s"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Receiver: tt.params}.ValidateReceiverEntity()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateReceiverEntity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSenderEntity_RejectsBothQueueAndTopic pins on the
// sender build boundary (factory.NewSender → Config.ValidateSenderEntity).
//
// Mutation: drop the validateSenderEntityExclusive call. Then the both-set
// case returns nil and the wantErr assertion FAILS.
func TestValidateSenderEntity_RejectsBothQueueAndTopic(t *testing.T) {
	tests := []struct {
		name    string
		params  SenderParams
		wantErr bool
	}{
		{"queue only", SenderParams{QueueName: "q"}, false},
		{"topic only", SenderParams{TopicName: "t"}, false},
		{"neither rejected", SenderParams{}, true},
		{"both queue and topic rejected", SenderParams{QueueName: "q", TopicName: "t"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Sender: tt.params}.ValidateSenderEntity()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSenderEntity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
