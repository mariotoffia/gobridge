package servicebus

import (
	"testing"
	"time"
)

// TestReceiverConfigFromOptions_StringDuration_MaxWaitTime verifies that
// ReceiverConfigFromOptions parses a string duration ("10s") for max_wait_time.
// This exposes the bug where opts["max_wait_time"].(time.Duration) silently
// drops string values.
func TestReceiverConfigFromOptions_StringDuration_MaxWaitTime(t *testing.T) {
	cfg := ReceiverConfigFromOptions(map[string]any{
		"queue_name":    "my-queue",
		"max_wait_time": "10s",
	})

	if cfg.MaxWaitTime != 10*time.Second {
		t.Errorf("MaxWaitTime = %v, want 10s (string duration must be parsed)", cfg.MaxWaitTime)
	}
}

// TestSenderConfigFromOptions_StringDuration_Timeout verifies that
// SenderConfigFromOptions parses a string duration ("5s") for timeout.
// This exposes the bug where opts["timeout"].(time.Duration) silently
// drops string values.
func TestSenderConfigFromOptions_StringDuration_Timeout(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{
		"queue_name": "my-queue",
		"timeout":    "5s",
	})

	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (string duration must be parsed)", cfg.Timeout)
	}
}
