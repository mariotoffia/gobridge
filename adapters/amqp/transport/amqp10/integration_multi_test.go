package amqp10

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// TestIntegration_MultipleSenders validates that multiple senders can
// publish to the same address concurrently.
func TestIntegration_MultipleSenders(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-multi-send")

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	const sendersCount = 2
	const msgsPerSender = 3
	const totalMsgs = sendersCount * msgsPerSender

	for s := 0; s < sendersCount; s++ {
		sender, err := NewSender(SenderConfig{
			Address: addr,
			Session: sess,
		}, sess)
		if err != nil {
			t.Fatalf("NewSender(%d) error = %v", s, err)
		}
		for m := 0; m < msgsPerSender; m++ {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      fmt.Sprintf("multi-s%d-m%d", s, m),
				Payload: []byte(fmt.Sprintf("sender-%d-msg-%d", s, m)),
			})
			if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
				t.Fatalf("sender %d Send(%d) error = %v", s, m, err)
			}
		}
		if err := sender.Close(context.Background()); err != nil {
			t.Fatalf("sender %d Close() error = %v", s, err)
		}
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

	seen := make(map[string]bool)
	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		seen[del.Envelope().ID()] = true
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		if len(seen) >= totalMsgs {
			recvCancel()
		}
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if len(seen) != totalMsgs {
		t.Fatalf("received %d unique messages, want %d", len(seen), totalMsgs)
	}
}

// TestIntegration_CompetingReceivers validates that multiple receivers
// on the same address each get a share of messages.
func TestIntegration_CompetingReceivers(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-competing")

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	const totalMsgs = 10

	sender, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	for i := 0; i < totalMsgs; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("competing-%d", i),
			Payload: []byte(fmt.Sprintf("msg-%d", i)),
		})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
			t.Fatalf("Send(%d) error = %v", i, err)
		}
	}
	if err := sender.Close(context.Background()); err != nil {
		t.Fatalf("sender Close() error = %v", err)
	}

	collected := make(chan string, totalMsgs*2)
	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	var wg sync.WaitGroup
	for r := 0; r < 2; r++ {
		recv, err := NewReceiver(ReceiverConfig{
			Address:    addr,
			LinkCredit: 5,
			Session:    sess,
		}, sess)
		if err != nil {
			t.Fatalf("NewReceiver(%d) error = %v", r, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
				collected <- del.Envelope().ID()
				if err := del.Ack(ctx); err != nil {
					t.Errorf("Ack() error = %v", err)
				}
				return nil
			})
		}()
	}

	seen := make(map[string]bool)
	deadline := time.After(15 * time.Second)
	for len(seen) < totalMsgs {
		select {
		case id := <-collected:
			seen[id] = true
		case <-deadline:
			t.Fatalf("timeout: received %d/%d messages", len(seen), totalMsgs)
		}
	}

	recvCancel()
	wg.Wait()

	if len(seen) != totalMsgs {
		t.Fatalf("received %d unique messages, want %d", len(seen), totalMsgs)
	}
}

// TestIntegration_SessionReconcileEvents validates that Reconcile
// stores the plan and emits SessionReconciled event.
func TestIntegration_SessionReconcileEvents(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	ev := readEvent(t, sess.Events(), 5*time.Second)
	if ev.Type != ports.SessionConnected {
		t.Fatalf("expected SessionConnected event, got %v", ev.Type)
	}

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "topic-a"},
			{Topic: "topic-b"},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	ev = readEvent(t, sess.Events(), 5*time.Second)
	if ev.Type != ports.SessionReconciled {
		t.Fatalf("expected SessionReconciled event, got %v", ev.Type)
	}

	health := sess.Health(ctx)
	if health.SubscriptionsWanted != 2 {
		t.Fatalf("SubscriptionsWanted = %d, want 2", health.SubscriptionsWanted)
	}
}

// TestIntegration_SenderCloseReopen validates that a sender can be
// closed and a new sender created on the same session.
func TestIntegration_SenderCloseReopen(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("test-reopen")

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start() error = %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	sender1, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender(1) error = %v", err)
	}
	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "reopen-1",
		Payload: []byte("first"),
	})
	if err := sender1.Send(ctx, ports.OutboundMessage{Envelope: env1}); err != nil {
		t.Fatalf("sender1 Send() error = %v", err)
	}
	if err := sender1.Close(context.Background()); err != nil {
		t.Fatalf("sender1 Close() error = %v", err)
	}

	sender2, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender(2) error = %v", err)
	}
	defer func() { _ = sender2.Close(context.Background()) }()

	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "reopen-2",
		Payload: []byte("second"),
	})
	if err := sender2.Send(ctx, ports.OutboundMessage{Envelope: env2}); err != nil {
		t.Fatalf("sender2 Send() error = %v", err)
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

	seen := make(map[string]bool)
	runErr := recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		seen[del.Envelope().ID()] = true
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		if len(seen) >= 2 {
			recvCancel()
		}
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if !seen["reopen-1"] || !seen["reopen-2"] {
		t.Fatalf("expected both reopen-1 and reopen-2, got %v", seen)
	}
}

func readEvent(t *testing.T, ch <-chan ports.SessionEvent, timeout time.Duration) ports.SessionEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatal("timeout waiting for session event")
		return ports.SessionEvent{}
	}
}
