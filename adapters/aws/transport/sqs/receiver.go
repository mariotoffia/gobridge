package sqs

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for Amazon SQS. It long-polls for
// messages and emits each as a ports.Delivery whose Ack/Retry/Extend
// operations map to the SQS receipt-handle lifecycle.
type Receiver struct {
	cfg         ReceiverConfig
	client      sqsAPI
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	initMu      sync.Mutex
	started     chan struct{}
	startedOnce sync.Once
}

// NewReceiver creates an SQS Receiver.
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

// Run starts the long-poll loop. For each received SQS message it
// creates a Delivery and calls emit synchronously, providing natural
// backpressure. Run blocks until ctx is cancelled or an unrecoverable
// error occurs.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	initCtx, initCancel := context.WithTimeout(ctx, r.cfg.InitTimeout)
	defer initCancel()

	if err := r.ensureClient(initCtx); err != nil {
		return err
	}

	queueURL, err := resolveQueueURL(initCtx, r.client, r.cfg.QueueURL, r.cfg.QueueName)
	if err != nil {
		return err
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver starting",
			"queue_url", queueURL,
			"max_messages", r.cfg.MaxMessages,
			"visibility_timeout", r.cfg.VisibilityTimeout,
			"auto_extend", r.cfg.autoExtendEnabled(),
		)
	}

	return r.pollLoop(ctx, queueURL, emit)
}

func (r *Receiver) pollLoop(
	ctx context.Context,
	queueURL string,
	emit func(context.Context, ports.Delivery) error,
) error {
	backoff := newPollBackoffFromConfig(r.cfg)
	pollTimeout := time.Duration(r.cfg.WaitTimeSeconds+10) * time.Second

	r.startedOnce.Do(func() { close(r.started) })

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		results, err := r.pollAndConvert(ctx, queueURL, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := backoff.next()
			if r.logger != nil {
				r.logger.Warn("sqs: ReceiveMessage failed, retrying",
					"queue", queueURL,
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

		backoff.reset()

		for _, raw := range results {
			// Per-delivery context so that auto-extend failure can
			// cancel processing without affecting other deliveries.
			// The cancel func is passed into newDelivery so it is set
			// BEFORE any auto-extend goroutine starts.
			deliveryCtx, deliveryCancel := context.WithCancel(ctx)
			del := newDelivery(
				deliveryCtx,
				raw.env,
				r.client,
				queueURL,
				raw.receiptHandle,
				r.cfg.VisibilityTimeout,
				r.cfg.autoExtendEnabled(),
				deliveryCancel,
				r.logger,
				r.metrics,
				r.clock(),
			)

			if err := emit(deliveryCtx, del); err != nil {
				deliveryCancel()
				return err
			}
		}
	}
}

// pollBackoff implements exponential backoff with jitter for poll loops.
type pollBackoff struct {
	initial    time.Duration
	max        time.Duration
	multiplier float64
	current    time.Duration
}

func newPollBackoffFromConfig(cfg ReceiverConfig) *pollBackoff {
	return &pollBackoff{
		initial:    cfg.PollBackoffInitial,
		max:        cfg.PollBackoffMax,
		multiplier: cfg.PollBackoffMultiplier,
		current:    cfg.PollBackoffInitial,
	}
}

func (b *pollBackoff) next() time.Duration {
	delay := b.current

	jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
	delay += jitter

	b.current = time.Duration(float64(b.current) * b.multiplier)
	if b.current > b.max {
		b.current = b.max
	}

	return delay
}

func (b *pollBackoff) reset() {
	b.current = b.initial
}
