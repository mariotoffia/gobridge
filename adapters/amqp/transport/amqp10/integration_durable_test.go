// Validates durable subscriptions end-to-end (finding: durability_mode
// previously configured only the client terminus and links had random
// names, so a durable subscriber lost everything published while
// detached). The broker must retain messages published to a multicast
// address while the durable subscriber is detached, and deliver them
// when a link with the SAME container-id + subscription name reattaches.
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
)

func TestIntegration_DurableSubscription_SurvivesDetach(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("durable-sub")

	// Durable subscriptions are identified by container-id + link name —
	// both MUST be stable across attaches (and across bridge restarts).
	const containerID = "gobridge-durable-sub-test"

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
			DurabilityMode:   2, // unsettled-state: durable subscription
			SubscriptionName: "durable-sub-test",
		}, sess)
		if err != nil {
			t.Fatalf("NewReceiver: %v", err)
		}
		return recv
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Phase 1: attach the durable subscriber so the broker creates the
	// subscription, then go away WITHOUT consuming anything — modelling
	// a bridge shutdown/restart (link abandoned, connection torn down;
	// a client-side link CLOSE would be an unsubscribe and is exactly
	// what the receiver must not do for durable modes).
	sess1 := newSess()
	recv1 := newDurableReceiver(sess1)
	attachCtx, attachCancel := context.WithCancel(ctx)
	recv1Done := make(chan error, 1)
	go func() {
		recv1Done <- recv1.Run(attachCtx, func(_ context.Context, del ports.Delivery) error {
			t.Errorf("unexpected delivery before any send: %s", del.Envelope().ID())
			_ = del.Ack(ctx)
			return nil
		})
	}()
	select {
	case <-recv1.Started():
	case <-ctx.Done():
		t.Fatal("timeout waiting for durable subscription to attach")
	}
	attachCancel()
	<-recv1Done
	if err := recv1.Close(context.Background()); err != nil {
		t.Fatalf("recv1 Close: %v", err)
	}
	if err := sess1.Close(context.Background()); err != nil {
		t.Fatalf("sess1 Close: %v", err)
	}

	// Phase 2: publish while the subscriber is DETACHED. The broker must
	// retain the message for the durable subscription.
	sess2 := newSess()
	defer func() { _ = sess2.Close(context.Background()) }()

	sender, err := NewSender(SenderConfig{
		Address: addr,
		Session: sess2,
		Routing: RoutingMulticast,
	}, sess2)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "durable-while-detached",
		Subject: "test.durable",
		Payload: []byte(`{"published":"while-detached"}`),
	})
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// Phase 3: reattach with the SAME container-id + subscription name
	// and require the retained message.
	recv2 := newDurableReceiver(sess2)
	defer func() { _ = recv2.Close(context.Background()) }()

	recvCtx, recvCancel := context.WithTimeout(ctx, 20*time.Second)
	defer recvCancel()

	var got *messaging.Envelope
	runErr := recv2.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		got = del.Envelope()
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("Ack() error = %v", err)
		}
		recvCancel()
		return nil
	})
	if runErr != nil && recvCtx.Err() == nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if got == nil {
		t.Fatal("durable subscription lost the message published while detached")
	}
	if got.ID() != "durable-while-detached" {
		t.Fatalf("received ID = %q, want %q", got.ID(), "durable-while-detached")
	}
}
