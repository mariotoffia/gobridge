package amqp091

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// TestIntegration_Receiver_GracefulStop_NoLeakedConsumer is the
// regression proof against a live broker: a DIRECT embedder (no route
// runner) that drives Run and stops it (ctx cancel) must NOT leave a
// consumer registered — even WITHOUT calling Receiver.Close — because a
// graceful stop self-closes the consumer channel.
//
// The deterministic signal is a stolen message rather than the management
// API consumer count (which deregisters too quickly to observe): with
// autoAck a leaked consumer whose forwarder goroutine has exited still
// receives — and the broker auto-acks into the void — the NEXT publish, so
// a second receiver on the same queue never sees it. This is the exact
// failure commit b9cd3af introduced for embedders driving Run directly.
//
// Counterfactual (verified by reverting the self-close): with the pre-fix
// always-hand-off behavior and no Receiver.Close, receiver2 TIMES OUT —
// leak-2 was stolen by the leaked consumer. With the self-close fix
// receiver2 receives leak-2.
func TestIntegration_Receiver_GracefulStop_NoLeakedConsumer(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("leak")
	exchangeName := rabbitmqlocal.UniqueExchange("leak-ex")

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, nil)
	require.NoError(t, sess.Start(ctx))
	defer func() { _ = sess.Close(ctx) }()

	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{
			Topic: queueName,
			Config: &Config{Subscription: SubscriptionParams{
				Exchange: exchangeName, RoutingKey: queueName,
			}},
		}},
		Publishers: []connectivity.PublisherPlan{{Topic: exchangeName}},
	}))

	sender := NewSender(SenderConfig{
		Exchange: exchangeName, RoutingKey: queueName, Session: sess, Timeout: 10 * time.Second,
	})
	publish := func(id string) {
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: id, Subject: queueName, Payload: []byte(id),
		})}))
	}

	// receiver1 consumes leak-1 and stops (ctx cancel). It NEVER calls
	// Close: the self-close on graceful stop is what must release the
	// consumer so a forgotten Close cannot leak it.
	publish("leak-1")
	receiver1 := NewReceiver(ReceiverConfig{QueueName: queueName, PrefetchCount: 1, AutoAck: true, Session: sess})
	r1ctx, r1cancel := context.WithCancel(ctx)
	defer r1cancel()
	got1 := make(chan struct{}, 1)
	run1Done := make(chan struct{})
	go func() {
		defer close(run1Done)
		_ = receiver1.Run(r1ctx, func(_ context.Context, _ ports.Delivery) error {
			got1 <- struct{}{}
			r1cancel() // graceful stop
			return nil
		})
	}()
	select {
	case <-got1:
	case <-ctx.Done():
		t.Fatal("timed out waiting for leak-1")
	}
	<-run1Done // Run returned; the direct-embedder self-close must have fired.

	// leak-2 must reach a FRESH receiver, not the (now self-closed) consumer1.
	publish("leak-2")
	receiver2 := NewReceiver(ReceiverConfig{QueueName: queueName, PrefetchCount: 1, AutoAck: true, Session: sess})
	r2ctx, r2cancel := context.WithTimeout(ctx, 10*time.Second)
	defer r2cancel()
	got2 := make(chan string, 1)
	go func() {
		_ = receiver2.Run(r2ctx, func(_ context.Context, d ports.Delivery) error {
			got2 <- d.Envelope().ID()
			r2cancel()
			return nil
		})
	}()
	select {
	case id := <-got2:
		require.Equal(t, "leak-2", id, "receiver2 received the wrong message")
	case <-r2ctx.Done():
		t.Fatal("receiver2 timed out: leak-2 was stolen by a leaked consumer (self-close regression)")
	}
	_ = receiver2.Close(ctx)
}
