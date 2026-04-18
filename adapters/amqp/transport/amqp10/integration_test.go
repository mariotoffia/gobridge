// Provides integration tests against a live Apache ActiveMQ Artemis broker.
package amqp10

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

func TestMain(m *testing.M) {
	artemislocal.Configure(artemislocal.WithCleanOrphans(true))
	code := m.Run()
	artemislocal.Shutdown()
	os.Exit(code)
}

func TestIntegration_SendReceive(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-sendrecv")

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 15 * time.Second,
		IdleTimeout:    1 * time.Minute,
	}, domain.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	sender, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess,
		Timeout: 10 * time.Second,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	env := &domain.Envelope{
		ID:        "integ-msg-1",
		Subject:   "test.integration",
		Payload:   []byte(`{"key":"value"}`),
		CreatedAt: time.Now().UTC(),
		Headers: map[string]any{
			"tenant": "test-tenant",
		},
	}

	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	recv, err := NewReceiver(ReceiverConfig{
		Address:    addr,
		LinkCredit: 10,
		Session:    sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	var received *domain.Envelope
	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		received = del.Envelope()
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		recvCancel()
		return nil
	})

	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if received == nil {
		t.Fatal("no message received")
	}
	if received.ID != "integ-msg-1" {
		t.Fatalf("received ID = %q, want %q", received.ID, "integ-msg-1")
	}
	if string(received.Payload) != `{"key":"value"}` {
		t.Fatalf("received Payload = %q", received.Payload)
	}
	if received.Headers["tenant"] != "test-tenant" {
		t.Fatalf("received Headers[tenant] = %v", received.Headers["tenant"])
	}
}

func TestIntegration_SendBatch(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-batch")

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 15 * time.Second,
	}, domain.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	sender, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	envs := []*domain.Envelope{
		{ID: "batch-1", Payload: []byte("one")},
		{ID: "batch-2", Payload: []byte("two")},
		{ID: "batch-3", Payload: []byte("three")},
	}

	sent, err := sender.SendBatch(ctx, envs)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if sent != 3 {
		t.Fatalf("sent = %d, want 3", sent)
	}

	recv, err := NewReceiver(ReceiverConfig{
		Address:    addr,
		LinkCredit: 10,
		Session:    sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	var received []*domain.Envelope
	_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		received = append(received, del.Envelope())
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack: %v", err)
		}
		if len(received) >= 3 {
			recvCancel()
		}
		return nil
	})

	if len(received) != 3 {
		t.Fatalf("received %d messages, want 3", len(received))
	}

	rxBodies := make(map[string]bool, 3)
	for _, env := range received {
		rxBodies[string(env.Payload)] = true
	}
	for _, want := range []string{"one", "two", "three"} {
		if !rxBodies[want] {
			t.Errorf("missing payload %q in received messages", want)
		}
	}
}

func TestIntegration_SessionHealth(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 15 * time.Second,
	}, domain.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	healthBefore := sess.Health(ctx)
	if healthBefore.Connected {
		t.Fatal("session should not be connected before Start()")
	}

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	healthAfter := sess.Health(ctx)
	if !healthAfter.Connected {
		t.Fatal("session should be connected after Start()")
	}
	if !healthAfter.Ready {
		t.Fatal("session should be ready after Start()")
	}
	if healthAfter.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("ServiceLevel = %q, want %q", healthAfter.ServiceLevel, ports.ServiceLevelFull)
	}
}
