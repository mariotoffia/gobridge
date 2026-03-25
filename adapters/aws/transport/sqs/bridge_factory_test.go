package sqs

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

func TestBridgeFactory_NewSession_ReturnsNilNil(t *testing.T) {
	bf := NewBridgeFactory(nil)

	sess, err := bf.NewSession(context.Background(), config.SessionDef{
		ID:        "sqs-session",
		Transport: "sqs",
	})

	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if sess != nil {
		t.Fatal("NewSession should return nil session for stateless SQS transport")
	}
}

func TestBridgeFactory_Capabilities(t *testing.T) {
	bf := NewBridgeFactory(nil)
	caps := bf.Capabilities()

	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(caps))
	}

	want := map[ports.Capability]bool{
		ports.CapVisibilityExtension: false,
		ports.CapSourceRedelivery:    false,
	}
	for _, c := range caps {
		if _, ok := want[c]; !ok {
			t.Errorf("unexpected capability: %q", c)
		}
		want[c] = true
	}
	for cap, found := range want {
		if !found {
			t.Errorf("missing capability: %q", cap)
		}
	}
}

func TestBridgeFactory_NewReceiver(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.ReceiverDef{
		ID:        "recv-1",
		Transport: "sqs",
		SessionID: "",
		Options: map[string]any{
			"queue_url":          "https://sqs.us-east-1.amazonaws.com/123456789/test-queue",
			"max_messages":       5,
			"wait_time_seconds":  10,
			"visibility_timeout": 60,
		},
	}

	recv, err := bf.NewReceiver(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

func TestBridgeFactory_NewSender(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.SenderDef{
		ID:        "send-1",
		Transport: "sqs",
		SessionID: "",
		Options: map[string]any{
			"queue_url":  "https://sqs.us-east-1.amazonaws.com/123456789/test-queue",
			"batch_size": 5,
			"fifo":       true,
		},
	}

	snd, err := bf.NewSender(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}

func TestBridgeFactory_NewReceiver_OptionsPassthrough(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.ReceiverDef{
		ID:        "recv-opts",
		Transport: "sqs",
		Options: map[string]any{
			"queue_url":          "https://sqs.us-east-1.amazonaws.com/123456789/q",
			"max_messages":       3,
			"visibility_timeout": 45,
			"sns_unwrap":         true,
		},
	}

	recv, err := bf.NewReceiver(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("expected non-nil receiver")
	}
}

func TestBridgeFactory_NewSender_OptionsPassthrough(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.SenderDef{
		ID:        "send-opts",
		Transport: "sqs",
		Options: map[string]any{
			"queue_url":        "https://sqs.us-east-1.amazonaws.com/123456789/q",
			"delay_seconds":    10,
			"message_group_id": "group-a",
		},
	}

	snd, err := bf.NewSender(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("expected non-nil sender")
	}
}
