package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

type fakeSession struct{}

func (fakeSession) Start(context.Context) error                        { return nil }
func (fakeSession) Reconcile(context.Context, domain.SessionPlan) error { return nil }
func (fakeSession) Health(context.Context) ports.SessionHealth          { return ports.SessionHealth{} }
func (fakeSession) Events() <-chan ports.SessionEvent                   { return nil }
func (fakeSession) Close(context.Context) error                        { return nil }

func TestBridgeFactory_Capabilities(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())
	caps := bf.Capabilities()

	if len(caps) != 2 {
		t.Fatalf("len(Capabilities) = %d, want 2", len(caps))
	}

	found := map[ports.Capability]bool{}
	for _, c := range caps {
		found[c] = true
	}

	if !found[ports.CapStatefulSession] {
		t.Error("missing CapStatefulSession")
	}
	if !found[ports.CapExclusiveIdentity] {
		t.Error("missing CapExclusiveIdentity")
	}
}

func TestBridgeFactory_NewSession_Success(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())

	sess, err := bf.NewSession(context.Background(), config.SessionDef{
		ID:        "test-session",
		Transport: "mqtt",
		Options: map[string]any{
			"broker_urls": []string{"tcp://localhost:1883"},
			"client_id":   "test-client",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess == nil {
		t.Fatal("session is nil")
	}
}

func TestBridgeFactory_NewSession_MissingClientID(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())

	_, err := bf.NewSession(context.Background(), config.SessionDef{
		ID:        "no-client-id",
		Transport: "mqtt",
		Options: map[string]any{
			"broker_urls": []string{"tcp://localhost:1883"},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

func TestBridgeFactory_NewSession_MissingBrokerURLs(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())

	_, err := bf.NewSession(context.Background(), config.SessionDef{
		ID:        "no-brokers",
		Transport: "mqtt",
		Options: map[string]any{
			"client_id": "test-client",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing broker_urls")
	}
}

func validSession() *Session {
	opts := DefaultSessionOptions()
	opts.BrokerURLs = []string{"tcp://localhost:1883"}
	opts.ClientID = "test-client"
	return NewSession(opts, "", slog.Default())
}

func TestBridgeFactory_NewReceiver_Success(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())
	sess := validSession()

	recv, err := bf.NewReceiver(context.Background(), config.ReceiverDef{
		ID:        "recv-1",
		SessionID: "test-session",
		Topics: []config.SubscriptionDef{
			{Topic: "sensors/+/data", QoS: 1},
		},
	}, sess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recv == nil {
		t.Fatal("receiver is nil")
	}
}

func TestBridgeFactory_NewReceiver_WrongSessionType(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())

	_, err := bf.NewReceiver(context.Background(), config.ReceiverDef{
		ID:        "recv-bad",
		SessionID: "test-session",
	}, fakeSession{})
	if err == nil {
		t.Fatal("expected error for non-paho session")
	}
}

func TestBridgeFactory_NewSender_Success(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())
	sess := validSession()

	sender, err := bf.NewSender(context.Background(), config.SenderDef{
		ID:        "sender-1",
		SessionID: "test-session",
		Options: map[string]any{
			"default_topic": "out/events",
		},
	}, sess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender == nil {
		t.Fatal("sender is nil")
	}
}

func TestBridgeFactory_NewSender_WrongSessionType(t *testing.T) {
	bf := NewBridgeFactory(slog.Default())

	_, err := bf.NewSender(context.Background(), config.SenderDef{
		ID:        "sender-bad",
		SessionID: "test-session",
	}, fakeSession{})
	if err == nil {
		t.Fatal("expected error for non-paho session")
	}
}
