package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies Factory.Capabilities returns the expected transport capabilities.
func TestFactory_Capabilities(t *testing.T) {
	f := NewFactory(slog.Default())
	caps := f.Capabilities()

	want := map[ports.Capability]bool{
		ports.CapStatefulSession:  false,
		ports.CapSourceRedelivery: false,
	}

	for _, c := range caps {
		want[c] = true
	}

	for cap, found := range want {
		if !found {
			t.Errorf("missing capability %q", cap)
		}
	}

	if len(caps) != 2 {
		t.Errorf("len(Capabilities) = %d, want 2", len(caps))
	}
}

// verifies Factory.NewSession rejects a missing broker_url with ErrInvalidPayload.
func TestFactory_NewSession_MissingBrokerURL(t *testing.T) {
	f := &Factory{Logger: slog.Default()}

	_, err := f.NewSession(context.Background(), ports.SessionSpec{
		ID:     "sess-1",
		Config: Config{},
	})
	if err == nil {
		t.Fatal("expected error for missing broker_url")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T: %v", err, err)
	}
	if !errors.Is(be, domain.ErrInvalidPayload) {
		t.Errorf("expected ErrInvalidPayload, got code %s", be.Code)
	}
}

// verifies Factory.NewSession via the unified ports.TransportFactory rejects invalid options.
func TestFactory_NewSession_Invalid(t *testing.T) {
	f := NewFactory(slog.Default())

	_, err := f.NewSession(context.Background(), ports.SessionSpec{
		ID:     "s1",
		Config: Config{},
	})
	if err == nil {
		t.Fatal("expected error for missing broker_url")
	}
}

// verifies ReceiverFactory.NewReceiver rejects a nil session.
func TestReceiverFactory_NewReceiver_NilSession(t *testing.T) {
	rf := NewReceiverFactory(slog.Default())

	_, err := rf.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "r1",
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies ReceiverFactory.NewReceiver rejects a non-AMQP session type.
func TestReceiverFactory_NewReceiver_WrongSessionType(t *testing.T) {
	rf := NewReceiverFactory(slog.Default())

	_, err := rf.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "r2",
	}, &fakeSession{})
	if err == nil {
		t.Fatal("expected error for wrong session type")
	}
}

// verifies ReceiverFactory.NewReceiver falls back to the first subscription topic as queue name.
func TestReceiverFactory_NewReceiver_QueueFromSubscription(t *testing.T) {
	rf := NewReceiverFactory(slog.Default())

	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, domain.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()
	r, err := rf.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "r3",
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "events-queue"},
		},
	}, sess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	recv, ok := r.(*Receiver)
	if !ok {
		t.Fatal("expected *Receiver")
	}
	if recv.cfg.QueueName != "events-queue" {
		t.Errorf("QueueName = %q, want %q", recv.cfg.QueueName, "events-queue")
	}
}

// verifies SenderFactory.NewSender rejects a nil session.
func TestSenderFactory_NewSender_NilSession(t *testing.T) {
	sf := NewSenderFactory(slog.Default())

	_, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID: "s1",
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil session")
	}
}

// verifies SenderFactory.NewSender rejects a non-AMQP session type.
func TestSenderFactory_NewSender_WrongSessionType(t *testing.T) {
	sf := NewSenderFactory(slog.Default())

	_, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID: "s2",
	}, &fakeSession{})
	if err == nil {
		t.Fatal("expected error for wrong session type")
	}
}

// verifies SenderFactory.NewSender wires config from options.
func TestSenderFactory_NewSender_Valid(t *testing.T) {
	sf := NewSenderFactory(slog.Default())

	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, domain.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()
	s, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID: "s3",
		Config: Config{
			Sender: SenderParams{
				Exchange:   "my-exchange",
				RoutingKey: "my.key",
			},
		},
	}, sess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sender, ok := s.(*Sender)
	if !ok {
		t.Fatal("expected *Sender")
	}
	if sender.cfg.Exchange != "my-exchange" {
		t.Errorf("Exchange = %q", sender.cfg.Exchange)
	}
	if sender.cfg.RoutingKey != "my.key" {
		t.Errorf("RoutingKey = %q", sender.cfg.RoutingKey)
	}
}

// fakeSession implements ports.Session for type-mismatch tests.
type fakeSession struct{}

func (f *fakeSession) Start(context.Context) error                         { return nil }
func (f *fakeSession) Reconcile(context.Context, domain.SessionPlan) error { return nil }
func (f *fakeSession) Health(context.Context) ports.SessionHealth          { return ports.SessionHealth{} }
func (f *fakeSession) Events() <-chan ports.SessionEvent                   { return nil }
func (f *fakeSession) Close(context.Context) error                         { return nil }
