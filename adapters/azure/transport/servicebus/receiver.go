package servicebus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

type Receiver struct {
	cfg    ReceiverConfig
	client asbAPI
	logger *slog.Logger
}

func NewReceiver(cfg ReceiverConfig, logger *slog.Logger) (*Receiver, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Receiver{cfg: cfg, logger: logger}, nil
}

func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if err := r.ensureClient(ctx); err != nil {
		return err
	}

	if closer, ok := r.client.(interface{ Close(context.Context) error }); ok {
		defer closer.Close(ctx) //nolint:errcheck
	}

	return r.pollLoop(ctx, emit)
}

func (r *Receiver) ensureClient(_ context.Context) error {
	if r.client != nil {
		return nil
	}
	if r.cfg.Client != nil {
		r.client = r.cfg.Client
		return nil
	}

	asbClient, err := buildClient(r.cfg.Connection)
	if err != nil {
		return domain.ErrUnavailable.Wrap(fmt.Errorf("servicebus receiver: build client: %w", err))
	}

	opts := &azservicebus.ReceiverOptions{}

	if r.cfg.ReceiveMode == "ReceiveAndDelete" {
		opts.ReceiveMode = azservicebus.ReceiveModeReceiveAndDelete
	}

	switch r.cfg.SubQueue {
	case "deadletter":
		opts.SubQueue = azservicebus.SubQueueDeadLetter
	case "transferdeadletter":
		opts.SubQueue = azservicebus.SubQueueTransfer
	}

	var recv *azservicebus.Receiver

	if r.cfg.QueueName != "" {
		recv, err = asbClient.NewReceiverForQueue(r.cfg.QueueName, opts)
	} else {
		recv, err = asbClient.NewReceiverForSubscription(r.cfg.TopicName, r.cfg.SubscriptionName, opts)
	}

	if err != nil {
		return domain.ErrUnavailable.Wrap(fmt.Errorf("servicebus receiver: create receiver: %w", err))
	}

	r.client = recv
	return nil
}

func (r *Receiver) pollLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		msgs, err := r.client.ReceiveMessages(ctx, r.cfg.MaxMessages, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if r.logger != nil {
				r.logger.Warn("servicebus: ReceiveMessages failed, retrying",
					"error", err,
				)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}

		for _, msg := range msgs {
			del := r.convertMessage(ctx, msg)

			if err := emit(ctx, del); err != nil {
				return err
			}
		}
	}
}

func (r *Receiver) convertMessage(ctx context.Context, msg *azservicebus.ReceivedMessage) *asbDelivery {
	subject := r.cfg.QueueName
	if subject == "" {
		subject = r.cfg.TopicName
	}
	if msg.Subject != nil {
		subject = *msg.Subject
	}

	headers := messageToHeaders(msg)

	env := &domain.Envelope{
		ID:        msg.MessageID,
		Subject:   subject,
		Payload:   msg.Body,
		Headers:   headers,
		CreatedAt: time.Now(),
	}

	if msg.ExpiresAt != nil {
		env.ExpiresAt = *msg.ExpiresAt
	}

	return newDelivery(
		ctx,
		env,
		r.client,
		msg,
		r.cfg.LockDuration,
		r.cfg.autoExtendEnabled(),
		r.logger,
	)
}
