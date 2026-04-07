package sqs

import (
	"testing"
	"time"
)

// TestReceiverConfigFromOptions_Defaults validates that ReceiverConfigFromOptions
// returns correct defaults when the options map is empty.
func TestReceiverConfigFromOptions_Defaults(t *testing.T) {
	cfg := ReceiverConfigFromOptions(map[string]any{})

	if cfg.MaxMessages != 10 {
		t.Fatalf("MaxMessages: got %d, want 10", cfg.MaxMessages)
	}
	if cfg.WaitTimeSeconds != 20 {
		t.Fatalf("WaitTimeSeconds: got %d, want 20", cfg.WaitTimeSeconds)
	}
	if cfg.VisibilityTimeout != 30 {
		t.Fatalf("VisibilityTimeout: got %d, want 30", cfg.VisibilityTimeout)
	}
	if cfg.AutoExtend != nil {
		t.Fatal("AutoExtend should be nil (defaults to true in applyDefaults)")
	}
	if cfg.SNSUnwrap {
		t.Fatal("SNSUnwrap should default to false")
	}
}

// TestReceiverConfigFromOptions_AllFields validates that all recognized option
// keys are mapped to the correct ReceiverConfig fields.
func TestReceiverConfigFromOptions_AllFields(t *testing.T) {
	autoExtend := true
	cfg := ReceiverConfigFromOptions(map[string]any{
		"queue_url":          "https://sqs.us-west-1.amazonaws.com/123456789/test",
		"queue_name":         "test-queue",
		"region":             "eu-west-1",
		"endpoint":           "http://localhost:4566",
		"profile":            "dev",
		"max_messages":       5,
		"wait_time_seconds":  10,
		"visibility_timeout": 60,
		"auto_extend":        autoExtend,
		"sns_unwrap":         true,
	})

	if cfg.QueueURL != "https://sqs.us-west-1.amazonaws.com/123456789/test" {
		t.Fatalf("QueueURL: got %q", cfg.QueueURL)
	}
	if cfg.QueueName != "test-queue" {
		t.Fatalf("QueueName: got %q", cfg.QueueName)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("Region: got %q", cfg.Region)
	}
	if cfg.Endpoint != "http://localhost:4566" {
		t.Fatalf("Endpoint: got %q", cfg.Endpoint)
	}
	if cfg.Profile != "dev" {
		t.Fatalf("Profile: got %q", cfg.Profile)
	}
	if cfg.MaxMessages != 5 {
		t.Fatalf("MaxMessages: got %d, want 5", cfg.MaxMessages)
	}
	if cfg.WaitTimeSeconds != 10 {
		t.Fatalf("WaitTimeSeconds: got %d, want 10", cfg.WaitTimeSeconds)
	}
	if cfg.VisibilityTimeout != 60 {
		t.Fatalf("VisibilityTimeout: got %d, want 60", cfg.VisibilityTimeout)
	}
	if cfg.AutoExtend == nil || !*cfg.AutoExtend {
		t.Fatal("AutoExtend should be true")
	}
	if !cfg.SNSUnwrap {
		t.Fatal("SNSUnwrap should be true")
	}
}

// TestSenderConfigFromOptions_Defaults validates that SenderConfigFromOptions
// returns correct defaults when the options map is empty.
func TestSenderConfigFromOptions_Defaults(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{})

	if cfg.DelaySeconds != 0 {
		t.Fatalf("DelaySeconds: got %d, want 0", cfg.DelaySeconds)
	}
	if cfg.BatchSize != 10 {
		t.Fatalf("BatchSize: got %d, want 10", cfg.BatchSize)
	}
	if cfg.Timeout != 0 {
		t.Fatalf("Timeout: got %v, want 0 (applied later by applyDefaults)", cfg.Timeout)
	}
	if cfg.FIFO {
		t.Fatal("FIFO should default to false")
	}
	if cfg.MessageGroupID != "" {
		t.Fatalf("MessageGroupID should be empty, got %q", cfg.MessageGroupID)
	}
}

// TestSenderConfigFromOptions_AllFields validates that all recognized sender
// option keys are mapped correctly.
func TestSenderConfigFromOptions_AllFields(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{
		"queue_url":        "https://sqs.us-west-1.amazonaws.com/123456789/test.fifo",
		"queue_name":       "test-queue",
		"region":           "ap-southeast-1",
		"endpoint":         "http://localhost:4566",
		"profile":          "staging",
		"delay_seconds":    15,
		"batch_size":       5,
		"timeout":          10 * time.Second,
		"message_group_id": "group-1",
		"fifo":             true,
	})

	if cfg.QueueURL != "https://sqs.us-west-1.amazonaws.com/123456789/test.fifo" {
		t.Fatalf("QueueURL: got %q", cfg.QueueURL)
	}
	if cfg.QueueName != "test-queue" {
		t.Fatalf("QueueName: got %q", cfg.QueueName)
	}
	if cfg.Region != "ap-southeast-1" {
		t.Fatalf("Region: got %q", cfg.Region)
	}
	if cfg.Endpoint != "http://localhost:4566" {
		t.Fatalf("Endpoint: got %q", cfg.Endpoint)
	}
	if cfg.Profile != "staging" {
		t.Fatalf("Profile: got %q", cfg.Profile)
	}
	if cfg.DelaySeconds != 15 {
		t.Fatalf("DelaySeconds: got %d, want 15", cfg.DelaySeconds)
	}
	if cfg.BatchSize != 5 {
		t.Fatalf("BatchSize: got %d, want 5", cfg.BatchSize)
	}
	if cfg.Timeout != 10*time.Second {
		t.Fatalf("Timeout: got %v, want 10s", cfg.Timeout)
	}
	if cfg.MessageGroupID != "group-1" {
		t.Fatalf("MessageGroupID: got %q", cfg.MessageGroupID)
	}
	if !cfg.FIFO {
		t.Fatal("FIFO should be true")
	}
}

// TestSenderConfigFromOptions_FIFODetection validates that isFIFO detects FIFO
// mode from MessageGroupID or explicit FIFO flag.
func TestSenderConfigFromOptions_FIFODetection(t *testing.T) {
	tests := []struct {
		name string
		opts map[string]any
		want bool
	}{
		{
			name: "no FIFO indicators",
			opts: map[string]any{"queue_url": "https://sqs.example.com/q"},
			want: false,
		},
		{
			name: "explicit FIFO flag",
			opts: map[string]any{"queue_url": "https://sqs.example.com/q.fifo", "fifo": true},
			want: true,
		},
		{
			name: "MessageGroupID implies FIFO",
			opts: map[string]any{"queue_url": "https://sqs.example.com/q.fifo", "message_group_id": "grp"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := SenderConfigFromOptions(tt.opts)
			if got := cfg.isFIFO(); got != tt.want {
				t.Fatalf("isFIFO() = %v, want %v", got, tt.want)
			}
		})
	}
}
