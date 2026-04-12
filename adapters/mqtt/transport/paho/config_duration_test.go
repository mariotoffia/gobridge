package paho

import (
	"testing"
	"time"
)

// TestSessionOptionsFromMap_StringDuration verifies that SessionOptionsFromMap
// parses a string duration ("3s") for connect_timeout. This exposes the bug
// where opts["connect_timeout"].(time.Duration) silently drops string values.
func TestSessionOptionsFromMap_StringDuration(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"connect_timeout": "3s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.ConnectTimeout != 3*time.Second {
		t.Errorf("ConnectTimeout = %v, want 3s (string duration must be parsed)", opts.ConnectTimeout)
	}
}

// TestSenderOptionsFromMap_StringDuration verifies that SenderOptionsFromMap
// parses a string duration ("5s") for timeout. This exposes the bug where
// opts["timeout"].(time.Duration) silently drops string values.
func TestSenderOptionsFromMap_StringDuration(t *testing.T) {
	opts, err := SenderOptionsFromMap(map[string]any{
		"timeout": "5s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (string duration must be parsed)", opts.Timeout)
	}
}
