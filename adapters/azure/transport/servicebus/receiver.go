package servicebus

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

type Receiver struct {
	cfg       ReceiverConfig
	client    asbAPI
	scheduler retryScheduler
	asbClient *azservicebus.Client
	logger    *slog.Logger
	initMu    sync.Mutex
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

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if closer, ok := r.client.(interface{ Close(context.Context) error }); ok {
			_ = closer.Close(closeCtx)
		}
		if r.scheduler != nil {
			if closer, ok := r.scheduler.(interface{ Close(context.Context) error }); ok {
				_ = closer.Close(closeCtx)
			}
		}
		if r.asbClient != nil {
			_ = r.asbClient.Close(closeCtx)
		}
	}()

	return r.pollLoop(ctx, emit)
}

func (r *Receiver) ensureClient(ctx context.Context) error {
	r.initMu.Lock()
	defer r.initMu.Unlock()

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

	entityName := r.cfg.QueueName
	if entityName == "" {
		entityName = r.cfg.TopicName
	}

	if r.cfg.SessionID != "" {
		sessOpts := &azservicebus.SessionReceiverOptions{}
		if r.cfg.ReceiveMode == "ReceiveAndDelete" {
			sessOpts.ReceiveMode = azservicebus.ReceiveModeReceiveAndDelete
		}

		var sessRecv *azservicebus.SessionReceiver
		if r.cfg.QueueName != "" {
			sessRecv, err = asbClient.AcceptSessionForQueue(ctx, r.cfg.QueueName, r.cfg.SessionID, sessOpts)
		} else {
			sessRecv, err = asbClient.AcceptSessionForSubscription(ctx, r.cfg.TopicName, r.cfg.SubscriptionName, r.cfg.SessionID, sessOpts)
		}
		if err != nil {
			_ = asbClient.Close(context.Background())
			return domain.ErrUnavailable.Wrap(fmt.Errorf("servicebus receiver: accept session %q: %w", r.cfg.SessionID, err))
		}
		r.client = &sessionReceiverAdapter{inner: sessRecv}
	} else {
		var recv *azservicebus.Receiver
		if r.cfg.QueueName != "" {
			recv, err = asbClient.NewReceiverForQueue(r.cfg.QueueName, opts)
		} else {
			recv, err = asbClient.NewReceiverForSubscription(r.cfg.TopicName, r.cfg.SubscriptionName, opts)
		}
		if err != nil {
			_ = asbClient.Close(context.Background())
			return domain.ErrUnavailable.Wrap(fmt.Errorf("servicebus receiver: create receiver: %w", err))
		}
		r.client = recv
	}

	sender, err := asbClient.NewSender(entityName, nil)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("servicebus: could not create retry scheduler sender",
				"entity", entityName, "error", err)
		}
	} else {
		r.scheduler = sender
	}

	r.asbClient = asbClient
	return nil
}

func (r *Receiver) pollLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	backoff := newPollBackoff()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		msgs, err := r.client.ReceiveMessages(ctx, r.cfg.MaxMessages, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := backoff.next()
			if r.logger != nil {
				r.logger.Warn("servicebus: ReceiveMessages failed, retrying",
					"error", err,
					"retry_after", delay,
				)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		backoff.reset()

		for _, msg := range msgs {
			del := r.convertMessage(ctx, msg)

			if err := emit(ctx, del); err != nil {
				return err
			}
		}
	}
}

// pollBackoff implements exponential backoff with jitter for poll loops.
type pollBackoff struct {
	current time.Duration
}

const (
	pollBackoffInitial    = time.Second
	pollBackoffMax        = 30 * time.Second
	pollBackoffMultiplier = 2
)

func newPollBackoff() *pollBackoff {
	return &pollBackoff{current: pollBackoffInitial}
}

func (b *pollBackoff) next() time.Duration {
	delay := b.current

	jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
	delay += jitter

	b.current *= pollBackoffMultiplier
	if b.current > pollBackoffMax {
		b.current = pollBackoffMax
	}

	return delay
}

func (b *pollBackoff) reset() {
	b.current = pollBackoffInitial
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
		r.scheduler,
		msg,
		r.cfg.LockDuration,
		r.cfg.autoExtendEnabled(),
		r.logger,
	)
}
