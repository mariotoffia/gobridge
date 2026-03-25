package servicebus

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies NewSession returns a nil session with no error for stateless Service Bus transport.
func TestBridgeFactory_NewSession_ReturnsNilNil(t *testing.T) {
	bf := NewBridgeFactory(nil)

	sess, err := bf.NewSession(context.Background(), config.SessionDef{
		ID:        "asb-session",
		Transport: "servicebus",
	})

	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if sess != nil {
		t.Fatal("NewSession should return nil session for stateless servicebus transport")
	}
}

// verifies BridgeFactory reports visibility extension capability.
func TestBridgeFactory_Capabilities(t *testing.T) {
	bf := NewBridgeFactory(nil)
	caps := bf.Capabilities()

	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}
	if caps[0] != ports.CapVisibilityExtension {
		t.Errorf("expected %q, got %q", ports.CapVisibilityExtension, caps[0])
	}
}

// verifies NewReceiver builds a queue receiver from ReceiverDef options.
func TestBridgeFactory_NewReceiver(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.ReceiverDef{
		ID:        "recv-1",
		Transport: "servicebus",
		Options: map[string]any{
			"queue_name":        "test-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
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

// verifies NewSender builds a queue sender from SenderDef options.
func TestBridgeFactory_NewSender(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.SenderDef{
		ID:        "send-1",
		Transport: "servicebus",
		Options: map[string]any{
			"queue_name":        "test-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
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

// verifies NewReceiver builds a topic subscription receiver from options.
func TestBridgeFactory_NewReceiver_TopicSubscription(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.ReceiverDef{
		ID:        "recv-topic",
		Transport: "servicebus",
		Options: map[string]any{
			"topic_name":        "test-topic",
			"subscription_name": "test-sub",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
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

// verifies NewSender builds a topic sender from options.
func TestBridgeFactory_NewSender_Topic(t *testing.T) {
	bf := NewBridgeFactory(nil)

	def := config.SenderDef{
		ID:        "send-topic",
		Transport: "servicebus",
		Options: map[string]any{
			"topic_name":        "test-topic",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
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

// verifies ReceiverFactory.NewReceiver from a ports.ReceiverSpec.
func TestReceiverFactory_NewReceiver(t *testing.T) {
	rf := NewReceiverFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-direct",
		Options: map[string]any{
			"queue_name":        "direct-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	recv, err := rf.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies SenderFactory.NewSender from a ports.SenderSpec.
func TestSenderFactory_NewSender(t *testing.T) {
	sf := NewSenderFactory()

	spec := ports.SenderSpec{
		ID: "send-direct",
		Options: map[string]any{
			"queue_name":        "direct-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	snd, err := sf.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}
