package servicebus

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// verifies NewSession returns a nil session with no error for stateless Service Bus transport.
func TestFactory_NewSession_ReturnsNilNil(t *testing.T) {
	f := NewFactory(nil)

	sess, err := f.NewSession(context.Background(), ports.SessionSpec{
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

// verifies Factory reports visibility extension capability.
func TestFactory_Capabilities(t *testing.T) {
	f := NewFactory(nil)
	caps := f.Capabilities()

	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}
	if caps[0] != ports.CapVisibilityExtension {
		t.Errorf("expected %q, got %q", ports.CapVisibilityExtension, caps[0])
	}
}

// verifies Factory.NewReceiver builds a queue receiver from spec options.
func TestFactory_NewReceiver(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-1",
		Options: map[string]any{
			"queue_name":        "test-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	recv, err := f.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies Factory.NewSender builds a queue sender from spec options.
func TestFactory_NewSender(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.SenderSpec{
		ID: "send-1",
		Options: map[string]any{
			"queue_name":        "test-queue",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	snd, err := f.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}

// verifies Factory.NewReceiver builds a topic subscription receiver from options.
func TestFactory_NewReceiver_TopicSubscription(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-topic",
		Options: map[string]any{
			"topic_name":        "test-topic",
			"subscription_name": "test-sub",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	recv, err := f.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies Factory.NewSender builds a topic sender from options.
func TestFactory_NewSender_Topic(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.SenderSpec{
		ID: "send-topic",
		Options: map[string]any{
			"topic_name":        "test-topic",
			"connection_string": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake",
		},
	}

	snd, err := f.NewSender(context.Background(), spec, nil)
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
	sf := NewSenderFactory(nil)

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
