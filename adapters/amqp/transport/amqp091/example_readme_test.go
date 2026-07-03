package amqp091_test

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// Example mirrors the "Usage Example" in README.md so the documented
// code always compiles against the real API. It has no "Output:"
// comment, so `go test` compiles it but never executes it (it would
// need a live broker).
func Example() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create and start a session. Start dials AND reconciles: a nil
	// return means the connection is up and the declared topology (if a
	// plan was installed) is in place.
	sess := amqp091.NewSession(amqp091.SessionOptions{
		BrokerURL:      "amqp://localhost:5672/",
		Heartbeat:      10 * time.Second,
		ConnectTimeout: 30 * time.Second,
	}, connectivity.SessionEphemeral, logger)

	if err := sess.Start(ctx); err != nil {
		logger.Error("session start failed", "error", err)
		return
	}
	defer func() { _ = sess.Close(ctx) }()

	// Declare exchange, queue, and binding (plan-driven; re-applied on
	// every reconnect before the session reports Connected again).
	err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{
			Topic: "my-queue",
			Config: &amqp091.Config{Subscription: amqp091.SubscriptionParams{
				Exchange:     "my-exchange",
				ExchangeType: "direct",
				RoutingKey:   "events",
				Durable:      true,
			}},
		}},
	})
	if err != nil {
		logger.Error("reconcile failed", "error", err)
		return
	}

	// Create a sender. Publishes are persistent by default (survive a
	// broker restart on a durable queue); set DeliveryMode to
	// amqp091.DeliveryModeTransient to opt out.
	sender := amqp091.NewSender(amqp091.SenderConfig{
		Exchange:   "my-exchange",
		RoutingKey: "events",
		Session:    sess,
		Logger:     logger,
	})

	// Publish a message.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-1",
		Subject: "events",
		Payload: []byte(`{"event":"created"}`),
	})
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: "events"}); err != nil {
		logger.Error("send failed", "error", err)
	}

	// Create a receiver and consume.
	receiver := amqp091.NewReceiver(amqp091.ReceiverConfig{
		QueueName:     "my-queue",
		PrefetchCount: 10,
		Session:       sess,
		Logger:        logger,
	})

	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		e := del.Envelope()
		logger.Info("received", "id", e.ID(), "payload", string(e.Payload()))
		return del.Ack(ctx)
	})
}
