package sqs

import "testing"

// ═══════════════════════════════════════════════════════════════════════════
// SQS WaitTimeSeconds Default
//
// applyDefaults() must set WaitTimeSeconds to 20 when the value is 0, else
// the receiver short-polls.
// ═══════════════════════════════════════════════════════════════════════════

// TestApplyDefaults_WaitTimeSeconds_DefaultsTo20 pins that a zero or negative
// WaitTimeSeconds defaults to 20 (long polling) and a value over 20 is clamped.
func TestApplyDefaults_WaitTimeSeconds_DefaultsTo20(t *testing.T) {
	tests := []struct {
		name     string
		input    int32
		expected int32
		isBug    bool
	}{
		{"zero defaults to 20 (fixed)", 0, 20, false},
		{"negative defaults to 20", -1, 20, false},
		{"over 20 clamped", 25, 20, false},
		{"valid 10 unchanged", 10, 10, false},
		{"valid 20 unchanged", 20, 20, false},
		{"valid 1 unchanged", 1, 1, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ReceiverConfig{
				QueueURL:        "https://sqs.us-west-1.amazonaws.com/123/q",
				WaitTimeSeconds: tc.input,
			}
			cfg.applyDefaults()
			if cfg.WaitTimeSeconds != tc.expected {
				t.Errorf("WaitTimeSeconds = %d, want %d", cfg.WaitTimeSeconds, tc.expected)
			}
			if tc.isBug {
				t.Logf("WaitTimeSeconds=%d after applyDefaults() — should be 20", cfg.WaitTimeSeconds)
			}
		})
	}
}

// TestApplyDefaults_MaxMessages_CorrectlyDefaulted pins that MaxMessages is
// defaulted to 10 when zero.
func TestApplyDefaults_MaxMessages_CorrectlyDefaulted(t *testing.T) {
	cfg := ReceiverConfig{
		QueueURL:    "https://sqs.us-west-1.amazonaws.com/123/q",
		MaxMessages: 0,
	}
	cfg.applyDefaults()
	if cfg.MaxMessages != 10 {
		t.Errorf("MaxMessages = %d, want 10", cfg.MaxMessages)
	}
}
