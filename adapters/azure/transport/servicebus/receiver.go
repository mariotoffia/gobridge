package servicebus

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/logging"
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
	metrics   ports.MetricsExporter
	initMu    sync.Mutex
	closeOnce sync.Once
}

func NewReceiver(cfg ReceiverConfig, logger *slog.Logger) (*Receiver, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l := cfg.Logger
	if l == nil {
		l = logger
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	return &Receiver{cfg: cfg, logger: l, metrics: m}, nil
}

func (r *Receiver) entityName() string {
	if r.cfg.QueueName != "" {
		return r.cfg.QueueName
	}
	return r.cfg.TopicName
}

// Close releases the AMQP client, receiver, and scheduler resources.
// It is safe to call multiple times; only the first call performs cleanup.
// Callers must call Close after all outstanding deliveries have been
// settled (Ack/Retry) to avoid tearing down the AMQP link while
// settlement operations are still in progress.
func (r *Receiver) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		if closer, ok := r.client.(interface{ Close(context.Context) error }); ok {
			_ = closer.Close(ctx)
		}
		if r.scheduler != nil {
			if closer, ok := r.scheduler.(interface{ Close(context.Context) error }); ok {
				_ = closer.Close(ctx)
			}
		}
		if r.asbClient != nil {
			_ = r.asbClient.Close(ctx)
		}
	})
	return nil
}

func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if err := r.ensureClient(ctx); err != nil {
		return err
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "servicebus: receiver starting",
			"entity", r.entityName(),
			"max_messages", r.cfg.MaxMessages,
			"lock_duration", r.cfg.LockDuration,
			"auto_extend", r.cfg.autoExtendEnabled(),
		)
	}

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

	entityName := r.entityName()

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

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "servicebus: client initialized",
			"entity", entityName,
			"session_id", r.cfg.SessionID,
		)
	}

	return nil
}

func (r *Receiver) pollLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	backoff := newPollBackoff()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pollStart := time.Now()
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

		r.metrics.Timer(domain.MetricASBReceiveLatency, time.Since(pollStart),
			domain.Tag{Key: domain.TagKeyEntity, Value: r.entityName()})
		backoff.reset()

		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "servicebus: received",
				"entity", r.entityName(),
				"count", len(msgs),
			)
		}

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

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "servicebus: converting",
			"entity", r.entityName(),
			"message_id", msg.MessageID,
			"body_len", len(msg.Body),
		)
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
		r.metrics,
	)
}
