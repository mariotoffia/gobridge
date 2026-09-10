package paho

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestFactory_NewSession_MissingClientID validates that the factory returns
// an error when client_id is absent from the options map.
func TestFactory_NewSession_MissingClientID(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Config: Config{
			Session: SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}},
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestFactory_NewSession_MissingBrokerURLs validates that the factory returns
// an error when no broker URLs are provided.
func TestFactory_NewSession_MissingBrokerURLs(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Config: Config{
			Session: SessionOptions{ClientID: "test-client"},
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for missing broker URLs")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestFactory_NewSession_ValidOptions validates successful session creation
// when all required options are present.
func TestFactory_NewSession_ValidOptions(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Config: Config{
			Session: SessionOptions{
				ClientID:   "test-client",
				BrokerURLs: []string{"tcp://broker:1883"},
			},
		},
		SessionMode: connectivity.SessionEphemeral,
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

func TestFactory_NewSession_DurableModeRejectsIndependentBrokerURLs(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ClientID:   "durable-client",
		BrokerURLs: []string{"ssl://broker-a.example:8883", "ssl://broker-b.example:8883"},
	}}
	factory := NewFactory(nil)

	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		_, err := factory.NewSession(t.Context(), ports.SessionSpec{ID: "durable", SessionMode: mode, Config: cfg})
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("mode %s error = %v, want ErrInvalidConfig", mode, err)
		}
	}
	if _, err := factory.NewSession(t.Context(), ports.SessionSpec{
		ID: "ephemeral", SessionMode: connectivity.SessionEphemeral, Config: cfg,
	}); err != nil {
		t.Fatalf("ephemeral multi-broker failover must remain valid: %v", err)
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestFactory_NewSession_NilOptions validates that nil options map
// triggers a validation error for missing required fields.
func TestFactory_NewSession_NilOptions(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID:     "s-nil",
		Config: nil,
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for nil options")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestFactory_NewSession_EmptyBrokerURLs validates that an explicitly
// empty broker_urls list triggers a validation error.
func TestFactory_NewSession_EmptyBrokerURLs(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s-empty-urls",
		Config: Config{
			Session: SessionOptions{
				ClientID:   "test-client",
				BrokerURLs: []string{},
			},
		},
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for empty broker URLs")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestFactory_NewSession_BrokerURLSingular validates that the singular
// broker_url option key is accepted as a fallback.
func TestFactory_NewSession_BrokerURLSingular(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s-singular",
		Config: Config{
			Session: SessionOptions{
				ClientID:   "test-client",
				BrokerURLs: []string{"tcp://broker:1883"},
			},
		},
		SessionMode: connectivity.SessionEphemeral,
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
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
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestFactoryDedicatedReceiver_AdvertisesCapability(t *testing.T) {
	caps := NewFactory(nil).Capabilities()
	for _, cap := range caps {
		if cap == ports.CapDedicatedIngressSession {
			return
		}
	}
	t.Fatalf("Capabilities() = %v, want %q", caps, ports.CapDedicatedIngressSession)
}

func TestFactoryDedicatedReceiver_AliasesCannotReserveTwice(t *testing.T) {
	sess := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	firstAlias := NewFactory(nil)
	secondAlias := NewFactory(nil)
	spec := func(id, topic string) ports.ReceiverSpec {
		return ports.ReceiverSpec{
			ID:            id,
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
		}
	}

	first, err := firstAlias.NewReceiver(t.Context(), spec("receiver-a", "isolated/a"), sess)
	if err != nil {
		t.Fatalf("first alias NewReceiver: %v", err)
	}
	if first == nil {
		t.Fatal("first alias returned nil receiver")
	}
	_, err = secondAlias.NewReceiver(t.Context(), spec("receiver-b", "isolated/b"), sess)
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("second alias error = %v, want ErrInvalidConfig", err)
	}
}

func TestFactoryDedicatedReceiver_ConcurrentReservation(t *testing.T) {
	const callers = 16
	sess := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	factory := NewFactory(nil)
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := factory.NewReceiver(t.Context(), ports.ReceiverSpec{
				ID: fmt.Sprintf("receiver-%02d", i),
				Subscriptions: []connectivity.SubscriptionPlan{{
					Topic: fmt.Sprintf("isolated/%02d", i),
					QoS:   1,
				}},
			}, sess)
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("reservation error = %v, want ErrInvalidConfig", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful receiver reservations = %d, want 1", succeeded)
	}
}

func TestFactoryDedicatedReceiver_PreservesSenderSharing(t *testing.T) {
	sess := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	factory := NewFactory(nil)
	_, err := factory.NewReceiver(t.Context(), ports.ReceiverSpec{
		ID:            "receiver",
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "isolated/receiver", QoS: 1}},
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	cfg := Config{Sender: SenderOptions{QoS: 1}}
	for _, id := range []string{"sender-a", "sender-b"} {
		sender, err := factory.NewSender(t.Context(), ports.SenderSpec{ID: id, Config: cfg}, sess)
		if err != nil {
			t.Fatalf("NewSender %q: %v", id, err)
		}
		if sender == nil {
			t.Fatalf("NewSender %q returned nil", id)
		}
	}
}
