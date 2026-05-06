// Validates factory wiring for sessions, receivers, and senders.
package amqp10

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func TestFactory_Capabilities(t *testing.T) {
	bf := NewFactory(slog.Default())
	caps := bf.Capabilities()

	if len(caps) != 1 {
		t.Fatalf("Capabilities() returned %d caps, want 1", len(caps))
	}
	if caps[0] != ports.CapStatefulSession {
		t.Fatalf("Capabilities()[0] = %q, want %q", caps[0], ports.CapStatefulSession)
	}
}

func TestBridgeFactory_Capabilities_Contains_StatefulSession(t *testing.T) {
	bf := NewFactory(slog.Default())
	caps := bf.Capabilities()

	found := false
	for _, c := range caps {
		if c == ports.CapStatefulSession {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Capabilities() should include CapStatefulSession")
	}
}

func TestFactory_NewSession(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	spec := ports.SessionSpec{
		ID:          "sess-1",
		Transport:   "amqp10",
		SessionMode: domain.SessionEphemeral,
		Config: Config{
			Session: SessionOptions{Address: "amqp://localhost:5672"},
		},
	}

	sess, err := f.NewSession(context.Background(), spec)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if sess == nil {
		t.Fatal("NewSession() returned nil session")
	}

	amqpSess, ok := sess.(*Session)
	if !ok {
		t.Fatalf("session type = %T, want *Session", sess)
	}
	if amqpSess.opts.Address != "amqp://localhost:5672" {
		t.Fatalf("session address = %q", amqpSess.opts.Address)
	}
}

func TestFactory_NewSession_MissingAddress(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	spec := ports.SessionSpec{
		ID:     "sess-bad",
		Config: Config{},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("NewSession() should fail with missing address")
	}
}

func TestFactory_NewReceiver_InvalidSession(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	spec := ports.ReceiverSpec{
		ID:     "recv-1",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in"}},
	}

	_, err := f.NewReceiver(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("NewReceiver() should fail with nil session")
	}
}

func TestFactory_NewSender_InvalidSession(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	spec := ports.SenderSpec{
		ID:     "send-1",
		Config: Config{Sender: SenderParams{Address: "queue/out"}},
	}

	_, err := f.NewSender(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("NewSender() should fail with nil session")
	}
}

func TestFactory_NewReceiver_ValidSession(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())

	spec := ports.ReceiverSpec{
		ID:     "recv-1",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in"}},
	}

	recv, err := f.NewReceiver(context.Background(), spec, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver() returned nil")
	}
}

func TestFactory_NewSender_ValidSession(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())

	spec := ports.SenderSpec{
		ID:     "send-1",
		Config: Config{Sender: SenderParams{Address: "queue/out"}},
	}

	sender, err := f.NewSender(context.Background(), spec, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if sender == nil {
		t.Fatal("NewSender() returned nil")
	}
}
