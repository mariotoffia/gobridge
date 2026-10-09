// Validates that ending a durable subscription's broker state on close
// (ADR 0024) deletes the subscription on a real broker: the closing detach
// is an unsubscribe, so a message published after it is not kept, and a
// receiver that attaches again with the same container-id and subscription
// name starts on a new, empty subscription.
package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestIntegration_EndBrokerState_DeletesTheDurableSubscription(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("end-broker-state")
	const containerID = "gobridge-end-broker-state-test"

	newSess := func() *Session {
		sess := NewSession(SessionOptions{
			Address:        ep,
			Username:       user,
			Password:       shared.NewSecret(pass),
			ConnectTimeout: 15 * time.Second,
			ContainerID:    containerID,
		}, connectivity.SessionEphemeral, slog.Default())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sess.Start(ctx); err != nil {
			t.Fatalf("session Start() error = %v", err)
		}
		return sess
	}
	newDurableReceiver := func(sess *Session) *Receiver {
		recv, err := NewReceiver(ReceiverConfig{
			Address:          addr,
			LinkCredit:       10,
			Session:          sess,
			Routing:          RoutingMulticast,
			DurabilityMode:   2,
			SubscriptionName: "end-broker-state-sub",
			SessionID:        "orders",
		}, sess)
		if err != nil {
			t.Fatalf("NewReceiver: %v", err)
		}
		return recv
	}
	send := func(ctx context.Context, sender *Sender, id string) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: "test.end-broker-state", Payload: []byte(id)})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
			t.Fatalf("Send(%s) error = %v", id, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Attach so the broker creates the subscription, then retire the receiver
	// the way a reload whose next configuration drops it does: ask it to end
	// its broker state, stop its Run, and close it once Run has returned.
	sess1 := newSess()
	recv1 := newDurableReceiver(sess1)
	attachCtx, attachCancel := context.WithCancel(ctx)
	defer attachCancel()
	recv1Done := make(chan error, 1)
	go func() {
		recv1Done <- recv1.Run(attachCtx, func(_ context.Context, del ports.Delivery) error {
			t.Errorf("unexpected delivery before any send: %s", del.Envelope().ID())
			_ = del.Ack(ctx)
			return nil
		})
	}()
	wait.RequireClosed(t, recv1.Started(), 30*time.Second)
	recv1.EndBrokerStateOnClose()
	attachCancel()
	// Only that Run returns matters here, not its cancellation error.
	_ = wait.RequireReceive(t, recv1Done, 30*time.Second)
	if err := recv1.Close(context.Background()); err != nil {
		t.Fatalf("recv1 Close: %v", err)
	}
	if err := sess1.Close(context.Background()); err != nil {
		t.Fatalf("sess1 Close: %v", err)
	}

	// Published while nothing subscribes: an ended subscription keeps nothing.
	sess2 := newSess()
	defer func() { _ = sess2.Close(context.Background()) }()
	sender, err := NewSender(SenderConfig{Address: addr, Session: sess2, Routing: RoutingMulticast}, sess2)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()
	send(ctx, sender, "sent-after-end")

	// Attach again with the same identity: the broker creates a new, empty
	// subscription, so the first message it delivers is one sent after that.
	recv2 := newDurableReceiver(sess2)
	received := make(chan string, 4)
	recvCtx, recvCancel := context.WithCancel(ctx)
	recv2Done := make(chan error, 1)
	go func() {
		recv2Done <- recv2.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			received <- del.Envelope().ID()
			return del.Ack(recvCtx)
		})
	}()
	defer func() {
		// Leave no durable subscription behind on the shared broker.
		recv2.EndBrokerStateOnClose()
		recvCancel()
		_ = wait.RequireReceive(t, recv2Done, 30*time.Second)
		_ = recv2.Close(context.Background())
	}()
	wait.RequireClosed(t, recv2.Started(), 30*time.Second)
	send(ctx, sender, "sent-after-reattach")

	if first := wait.RequireReceive(t, received, 20*time.Second); first != "sent-after-reattach" {
		t.Fatalf("first delivery = %q; the ended subscription kept a message published after it ended", first)
	}
}
