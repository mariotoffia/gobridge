package sqs

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiverFactory_NewReceiver_OptionsPassthrough validates that
// ReceiverSpec.Options flow through the factory to ReceiverConfig fields.
func TestReceiverFactory_NewReceiver_OptionsPassthrough(t *testing.T) {
	f := NewReceiverFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "r1",
		Options: map[string]any{
			"queue_url":          "https://sqs.us-west-1.amazonaws.com/123/test",
			"max_messages":       5,
			"wait_time_seconds":  10,
			"visibility_timeout": 45,
		},
	}

	recv, err := f.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recv == nil {
		t.Fatal("receiver should not be nil")
	}

	r, ok := recv.(*Receiver)
	if !ok {
		t.Fatal("expected *Receiver")
	}
	if r.cfg.QueueURL != "https://sqs.us-west-1.amazonaws.com/123/test" {
		t.Fatalf("QueueURL not passed through: got %q", r.cfg.QueueURL)
	}
	if r.cfg.MaxMessages != 5 {
		t.Fatalf("MaxMessages: got %d, want 5", r.cfg.MaxMessages)
	}
}

// TestSenderFactory_NewSender_OptionsPassthrough validates that
// SenderSpec.Options flow through the factory to SenderConfig fields.
func TestSenderFactory_NewSender_OptionsPassthrough(t *testing.T) {
	f := NewSenderFactory(nil)

	spec := ports.SenderSpec{
		ID: "s1",
		Options: map[string]any{
			"queue_url":        "https://sqs.us-west-1.amazonaws.com/123/test.fifo",
			"batch_size":       3,
			"delay_seconds":    10,
			"message_group_id": "grp-a",
			"fifo":             true,
		},
	}

	sender, err := f.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender == nil {
		t.Fatal("sender should not be nil")
	}

	s, ok := sender.(*Sender)
	if !ok {
		t.Fatal("expected *Sender")
	}
	if s.cfg.QueueURL != "https://sqs.us-west-1.amazonaws.com/123/test.fifo" {
		t.Fatalf("QueueURL not passed through: got %q", s.cfg.QueueURL)
	}
	if s.cfg.BatchSize != 3 {
		t.Fatalf("BatchSize: got %d, want 3", s.cfg.BatchSize)
	}
	if !s.cfg.isFIFO() {
		t.Fatal("expected FIFO mode")
	}
}
