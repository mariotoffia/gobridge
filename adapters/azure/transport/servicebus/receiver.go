package servicebus

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver is the Service Bus inbound port adapter. All SDK access is
// concentrated in acl_*.go: this file references only the unexported
// seam interfaces (asbAPI, retryScheduler) and the *asbClientHandle
// wrapper, which makes the package's SDK boundary visible by file name.
type Receiver struct {
	cfg         ReceiverConfig
	client      asbAPI
	scheduler   retryScheduler
	asbClient   *asbClientHandle
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	initMu      sync.Mutex
	closeOnce   sync.Once
	started     chan struct{}
	startedOnce sync.Once
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
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	return &Receiver{cfg: cfg, logger: l, metrics: m, clk: clk, started: make(chan struct{})}, nil
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the receiver's poll
// loop is live and ready to process messages. It satisfies
// ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

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
		if closer, ok := r.client.(ports.ContextCloser); ok {
			_ = closer.Close(ctx)
		}
		if r.scheduler != nil {
			if closer, ok := r.scheduler.(ports.ContextCloser); ok {
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
		return err
	}

	recvOpts := asbReceiverOptions{
		ReceiveAndDelete: r.cfg.ReceiveMode == "ReceiveAndDelete",
		SubQueue:         r.cfg.SubQueue,
	}
	entityName := r.entityName()

	if r.cfg.SessionID != "" {
		sessOpts := asbSessionOptions{
			ReceiveAndDelete: r.cfg.ReceiveMode == "ReceiveAndDelete",
		}

		var seam asbAPI
		if r.cfg.QueueName != "" {
			seam, err = asbClient.AcceptSessionForQueue(ctx, r.cfg.QueueName, r.cfg.SessionID, sessOpts)
		} else {
			seam, err = asbClient.AcceptSessionForSubscription(ctx, r.cfg.TopicName, r.cfg.SubscriptionName, r.cfg.SessionID, sessOpts)
		}
		if err != nil {
			_ = asbClient.Close(context.Background())
			return shared.ErrUnavailable.Wrap(err)
		}
		r.client = seam
	} else {
		var seam asbAPI
		if r.cfg.QueueName != "" {
			seam, err = asbClient.NewReceiverForQueue(r.cfg.QueueName, recvOpts)
		} else {
			seam, err = asbClient.NewReceiverForSubscription(r.cfg.TopicName, r.cfg.SubscriptionName, recvOpts)
		}
		if err != nil {
			_ = asbClient.Close(context.Background())
			return shared.ErrUnavailable.Wrap(err)
		}
		r.client = seam
	}

	sender, err := asbClient.NewSender(entityName)
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

	r.startedOnce.Do(func() { close(r.started) })

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pollStart := r.clock().Now()
		deliveries, err := r.receiveAndConvert(ctx)
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
			case <-r.clock().After(delay):
			}
			continue
		}

		r.metrics.Timer(MetricASBReceiveLatency, r.clock().Since(pollStart),
			shared.Tag{Key: shared.TagKeyEntity, Value: r.entityName()})
		backoff.reset()

		for _, del := range deliveries {
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
