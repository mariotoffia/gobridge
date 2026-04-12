package sqs

import (
	"testing"
	"time"
)

// TestSenderConfigFromOptions_StringDuration verifies that SenderConfigFromOptions
// parses a string duration ("5s") into time.Duration. This exposes the bug where
// the type assertion opts["timeout"].(time.Duration) silently drops string values.
func TestSenderConfigFromOptions_StringDuration(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{
		"queue_url": "https://sqs.us-east-1.amazonaws.com/123/q",
		"timeout":   "5s",
	})

	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (string duration must be parsed)", cfg.Timeout)
	}
}

// TestSenderConfigFromOptions_NumericDuration verifies that SenderConfigFromOptions
// handles float64 timeout values (as produced by JSON unmarshal). This exposes the
// bug where only time.Duration type assertions succeed, dropping numeric seconds.
func TestSenderConfigFromOptions_NumericDuration(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{
		"queue_url": "https://sqs.us-east-1.amazonaws.com/123/q",
		"timeout":   30.0,
	})

	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s (float64 seconds must be converted)", cfg.Timeout)
	}
}
