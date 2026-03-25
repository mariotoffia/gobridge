package paho

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// TestFactory_NewSession_MissingClientID validates that the factory returns
// an error when client_id is absent from the options map.
func TestFactory_NewSession_MissingClientID(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Options: map[string]any{
			"broker_urls": []string{"tcp://broker:1883"},
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSession_MissingBrokerURLs validates that the factory returns
// an error when no broker URLs are provided.
func TestFactory_NewSession_MissingBrokerURLs(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Options: map[string]any{
			"client_id": "test-client",
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for missing broker URLs")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSession_ValidOptions validates successful session creation
// when all required options are present.
func TestFactory_NewSession_ValidOptions(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Options: map[string]any{
			"client_id":   "test-client",
			"broker_urls": []string{"tcp://broker:1883"},
		},
		SessionMode: domain.SessionEphemeral,
	}

	session, err := f.NewSession(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session == nil {
		t.Fatal("session should not be nil")
	}

	mqttSession, ok := session.(*Session)
	if !ok {
		t.Fatal("expected *Session")
	}
	if mqttSession == nil {
		t.Fatal("mqtt session should not be nil")
	}
}

// TestFactory_NewReceiver_WrongSessionType validates that the factory returns
// an error when the session is not an MQTT *Session.
func TestFactory_NewReceiver_WrongSessionType(t *testing.T) {
	f := &Factory{}
	spec := ports.ReceiverSpec{ID: "r1"}

	_, err := f.NewReceiver(context.Background(), spec, fakeSession{})
	if err == nil {
		t.Fatal("expected error for wrong session type")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSender_WrongSessionType validates that the factory returns
// an error when the session is not an MQTT *Session.
func TestFactory_NewSender_WrongSessionType(t *testing.T) {
	f := &Factory{}
	spec := ports.SenderSpec{ID: "s1"}

	_, err := f.NewSender(context.Background(), spec, fakeSession{})
	if err == nil {
		t.Fatal("expected error for wrong session type")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}
