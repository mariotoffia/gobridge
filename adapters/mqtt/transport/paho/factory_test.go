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

// TestFactory_NewSession_NilOptions validates that nil options map
// triggers a validation error for missing required fields.
func TestFactory_NewSession_NilOptions(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID:      "s-nil",
		Options: nil,
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for nil options")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSession_EmptyBrokerURLs validates that an explicitly
// empty broker_urls list triggers a validation error.
func TestFactory_NewSession_EmptyBrokerURLs(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s-empty-urls",
		Options: map[string]any{
			"client_id":   "test-client",
			"broker_urls": []string{},
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for empty broker URLs")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSession_BrokerURLSingular validates that the singular
// broker_url option key is accepted as a fallback.
func TestFactory_NewSession_BrokerURLSingular(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s-singular",
		Options: map[string]any{
			"client_id":  "test-client",
			"broker_url": "tcp://broker:1883",
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
}

// TestFactory_NewReceiver_NilSession validates that passing nil session
// to NewReceiver returns an error.
func TestFactory_NewReceiver_NilSession(t *testing.T) {
	f := &Factory{}
	spec := ports.ReceiverSpec{ID: "r-nil"}

	_, err := f.NewReceiver(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSender_NilSession validates that passing nil session
// to NewSender returns an error.
func TestFactory_NewSender_NilSession(t *testing.T) {
	f := &Factory{}
	spec := ports.SenderSpec{ID: "s-nil"}

	_, err := f.NewSender(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewReceiver_TypedNilSession validates that passing a typed nil
// *Session (non-nil interface wrapping nil value) returns an error.
func TestFactory_NewReceiver_TypedNilSession(t *testing.T) {
	f := &Factory{}
	var s *Session
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "r-tn"}, s)
	if err == nil {
		t.Fatal("expected error for typed nil session")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// TestFactory_NewSender_TypedNilSession validates that passing a typed nil
// *Session (non-nil interface wrapping nil value) returns an error.
func TestFactory_NewSender_TypedNilSession(t *testing.T) {
	f := &Factory{}
	var s *Session
	_, err := f.NewSender(context.Background(), ports.SenderSpec{ID: "s-tn"}, s)
	if err == nil {
		t.Fatal("expected error for typed nil session")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}
