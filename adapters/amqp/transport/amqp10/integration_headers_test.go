package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// TestIntegration_HeaderRoundTrip validates that all AMQP 1.0 message
// properties are preserved through a send/receive cycle.
func TestIntegration_HeaderRoundTrip(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-headers")

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

	env := &messaging.Envelope{
		ID:      "headers-rt-1",
		Subject: "test.headers",
		Payload: []byte(`{"test":"headers"}`),
		Headers: map[string]any{
			headerCorrelationID: "corr-123",
			headerContentType:   "application/json",
			headerSubject:       "test.headers",
			headerTo:            "destination-queue",
			headerReplyTo:       "reply-queue",
			headerGroupID:       "group-1",
			"x-custom-key":      "custom-value",
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

	var received *messaging.Envelope
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

	h := received.Headers
	assertHeader(t, h, headerCorrelationID, "corr-123")
	assertHeader(t, h, headerContentType, "application/json")
	assertHeader(t, h, headerSubject, "test.headers")
	assertHeader(t, h, headerTo, "destination-queue")
	assertHeader(t, h, headerReplyTo, "reply-queue")
	assertHeader(t, h, headerGroupID, "group-1")
	assertHeader(t, h, "x-custom-key", "custom-value")
}

// TestIntegration_EnvelopeTTL validates that envelope expiry is
// preserved via AbsoluteExpiryTime through send/receive.
func TestIntegration_EnvelopeTTL(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-ttl")

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

	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	env := &messaging.Envelope{
		ID:        "ttl-1",
		Subject:   "test.ttl",
		Payload:   []byte(`{"test":"ttl"}`),
		ExpiresAt: expiry,
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

	var received *messaging.Envelope
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

	if _, ok := received.Headers[headerAbsoluteExpiry]; !ok {
		t.Fatal("expected absolute-expiry-time header")
	}
	if received.ExpiresAt.IsZero() {
		t.Fatal("expected non-zero ExpiresAt on received envelope")
	}
	diff := received.ExpiresAt.Sub(expiry).Abs()
	if diff > 2*time.Second {
		t.Fatalf("ExpiresAt drift = %v (received=%v, sent=%v)", diff, received.ExpiresAt, expiry)
	}
}

// TestIntegration_ApplicationProperties validates that custom
// application properties round-trip and reserved headers are filtered.
func TestIntegration_ApplicationProperties(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-appprops")

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

	env := &messaging.Envelope{
		ID:      "appprops-1",
		Subject: "test.appprops",
		Payload: []byte(`{"test":"appprops"}`),
		Headers: map[string]any{
			"tenant":              "acme",
			"region":              "eu-west-1",
			"priority":            "high",
			"x-bridge.route-id":   "should-be-stripped",
			"x-bridge.session-id": "should-be-stripped",
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

	var received *messaging.Envelope
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

	h := received.Headers
	assertHeader(t, h, "tenant", "acme")
	assertHeader(t, h, "region", "eu-west-1")
	assertHeader(t, h, "priority", "high")

	for k := range h {
		if messaging.IsReservedHeader(k) {
			t.Errorf("reserved header %q present in received envelope", k)
		}
	}
}

func assertHeader(t *testing.T, h map[string]any, key string, want any) {
	t.Helper()
	got, ok := h[key]
	if !ok {
		t.Errorf("header %q not found", key)
		return
	}
	if got != want {
		t.Errorf("header %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
