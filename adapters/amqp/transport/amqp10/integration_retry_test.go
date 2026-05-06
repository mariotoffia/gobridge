package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// TestIntegration_RetryRelease validates that ReleaseMessage makes the
// message available for immediate redelivery.
func TestIntegration_RetryRelease(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-retry-release")

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

	env := &domain.Envelope{
		ID:      "retry-release-1",
		Subject: "test.retry.release",
		Payload: []byte(`{"action":"release"}`),
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

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	var attempt int
	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		attempt++
		if attempt == 1 {
			if err := del.Retry(recvCtx, 0, nil); err != nil {
				t.Errorf("Retry(release) error = %v", err)
			}
			return nil
		}
		headers := del.Envelope().Headers
		if dc, ok := headers[headerDeliveryCount]; ok {
			if count, ok := dc.(uint32); ok && count == 0 {
				t.Error("expected delivery-count > 0 on redelivery")
			}
		}
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		recvCancel()
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if attempt < 2 {
		t.Fatalf("expected at least 2 delivery attempts, got %d", attempt)
	}
}

// TestIntegration_RetryModify validates that ModifyMessage with
// DeliveryFailed=true marks the message for redelivery.
func TestIntegration_RetryModify(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-retry-modify")

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

	env := &domain.Envelope{
		ID:      "retry-modify-1",
		Subject: "test.retry.modify",
		Payload: []byte(`{"action":"modify"}`),
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

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	retryReason := errors.New("transient failure")
	var attempt int
	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		attempt++
		if attempt == 1 {
			if err := del.Retry(recvCtx, time.Second, retryReason); err != nil {
				t.Errorf("Retry(modify) error = %v", err)
			}
			return nil
		}
		headers := del.Envelope().Headers
		if dc, ok := headers[headerDeliveryCount]; ok {
			if count, ok := dc.(uint32); ok && count == 0 {
				t.Error("expected delivery-count > 0 on redelivery")
			}
		}
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		recvCancel()
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if attempt < 2 {
		t.Fatalf("expected at least 2 delivery attempts, got %d", attempt)
	}
}

// TestIntegration_ExtendNotSupported validates that Extend returns
// shared.ErrNotSupported on a real delivery.
func TestIntegration_ExtendNotSupported(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-extend")

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

	env := &domain.Envelope{
		ID:      "extend-1",
		Subject: "test.extend",
		Payload: []byte(`{"action":"extend"}`),
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

	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		err := del.Extend(recvCtx, time.Now().Add(time.Minute))
		if !errors.Is(err, shared.ErrNotSupported) {
			t.Errorf("Extend() error = %v, want ErrNotSupported", err)
		}
		if ackErr := del.Ack(recvCtx); ackErr != nil {
			t.Errorf("Ack() error = %v", ackErr)
		}
		recvCancel()
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
}
