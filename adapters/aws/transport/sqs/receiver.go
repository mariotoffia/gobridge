package sqs

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for Amazon SQS. It long-polls for
// messages and emits each as a ports.Delivery whose Ack/Retry/Extend
// operations map to the SQS receipt-handle lifecycle.
//
// Concurrency: the SQS client is held in an atomic.Pointer so the poll
// loop reads it lock-free while ApplyCredentials swaps a rotated client
// underneath. In-flight deliveries keep the client snapshot captured at
// creation. initMu serialises lazy-init and credential swaps.
type Receiver struct {
	cfg         ReceiverConfig
	client      atomic.Pointer[sqsAPI]
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	authGrace   *authGrace
	initMu      sync.Mutex
	started     chan struct{}
	startedOnce sync.Once

	// authFailureCB is the reactive-recovery hook. The
	// CredentialRefresher injects a URI-bound callback via
	// SetAuthFailureCallback; reportAuthFailure invokes it when a live
	// ReceiveMessage poll is classified as shared.ErrNotAuthorized (a static-key
	// revocation past the auth-grace window), forcing an immediate re-resolve
	// rather than degrading the route on revoked keys until the next poll.
	// atomic.Pointer gives safe publication across the builder goroutine
	// (setter) and the poll goroutine (load).
	authFailureCB atomic.Pointer[func(error)]
}

// loadClient returns the current SQS client snapshot, or nil when unset.
func (r *Receiver) loadClient() sqsAPI {
	if p := r.client.Load(); p != nil {
		return *p
	}
	return nil
}

// storeClient atomically installs the SQS client snapshot. A nil client
// clears it (lazy-init reset / tests).
func (r *Receiver) storeClient(c sqsAPI) {
	if c == nil {
		r.client.Store(nil)
		return
	}
	r.client.Store(&c)
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
	return &Receiver{cfg: cfg, logger: l, metrics: m, clk: clk, authGrace: newAuthGrace(clk, authGraceWindow), started: make(chan struct{})}, nil
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the poll loop is live and
// ready to process messages. If Run returns before reaching the poll loop
// (an initialisation error), Started() is NOT closed — Run's returned error
// is the authoritative failure signal, matching the paho/amqp10/servicebus
// receivers. Readiness paths must therefore also honour the terminal/error
// path and not select solely on Started() (see runtime WaitRouteReady, which
// re-checks on a periodic timer and on ctx cancellation). It satisfies
// ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// signalStarted closes the Started channel exactly once. It is invoked only
// once the poll loop is live (never on an initialisation-failure return).
func (r *Receiver) signalStarted() {
	r.startedOnce.Do(func() { close(r.started) })
}

// Run starts the long-poll loop. For each received SQS message it
// creates a Delivery and calls emit synchronously, providing natural
// backpressure. Run blocks until ctx is cancelled or an unrecoverable
// error occurs.
//
// Started() is signalled only after successful initialisation, once the
// poll loop is live. An initialisation error returns without signalling
// Started() so a readiness probe never briefly observes a ready route for
// a receiver that failed to start.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	initCtx, initCancel := context.WithTimeout(ctx, r.cfg.InitTimeout)
	defer initCancel()

	if err := r.ensureClient(initCtx); err != nil {
		return err
	}

	queueURL, err := resolveQueueURL(initCtx, r.loadClient(), r.cfg.QueueURL, r.cfg.QueueName)
	if err != nil {
		return err
	}

	// Best-effort startup check that the source queue has a native redrive
	// policy (maxReceiveCount -> DLQ) so a malformed message the receiver
	// cannot convert eventually reaches a DLQ instead of hot-looping forever
	// (Chunk 13). Permission-gated: a GetQueueAttributes denial degrades
	// to a log, never a start failure. It DOES fail startup for one loss-
	// critical case: a poison_max_receives backstop that would preempt an
	// EXISTING native redrive policy (destructive delete before the DLQ
	// preserves the payload) — see checkRedrivePolicy.
	if err := r.checkRedrivePolicy(initCtx, r.loadClient(), queueURL); err != nil {
		return err
	}

	// FIFO ordering safety: a single ReceiveMessage may return multiple
	// messages from the same MessageGroupId. The runtime processes
	// deliveries concurrently, so returning a group's messages in one
	// batch would let them be reordered. Forcing MaxMessages=1 makes SQS
	// hand back at most one message per receive; because a FIFO group is
	// locked to its in-flight message until that message is deleted, the
	// next message of the same group is never released while one is being
	// processed — preserving per-group order without serialising in the
	// shared route runner. Detected from the resolved URL's `.fifo`
	// suffix so a QueueName-only config is covered after resolution.
	//
	// The effective value is kept in a local (Finding 10): mutating
	// r.cfg.MaxMessages would race a second (mis)use of Run against the
	// shared config and permanently pin a re-pointed queue to 1.
	maxMessages := r.cfg.MaxMessages
	if isFIFOQueue(queueURL) && maxMessages != 1 {
		if r.logger != nil {
			r.logger.Warn("sqs: forcing max_messages=1 for FIFO source to preserve per-group ordering",
				"queue_url", queueURL,
				"configured_max_messages", r.cfg.MaxMessages,
			)
		}
		maxMessages = 1
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver starting",
			"queue_url", queueURL,
			"max_messages", maxMessages,
			"visibility_timeout", r.cfg.VisibilityTimeout,
			"auto_extend", r.cfg.autoExtendEnabled(),
		)
	}

	return r.pollLoop(ctx, queueURL, maxMessages, emit)
}

func (r *Receiver) pollLoop(
	ctx context.Context,
	queueURL string,
	maxMessages int32,
	emit func(context.Context, ports.Delivery) error,
) error {
	backoff := newPollBackoffFromConfig(r.cfg)
	pollTimeout := time.Duration(r.cfg.WaitTimeSeconds+10) * time.Second

	r.signalStarted()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Snapshot the client ONCE per batch (Finding: c8-settle-client).
		// The SAME client must serve the receive AND every resulting
		// delivery's Ack/Retry/auto-extend: if ApplyCredentials swaps a
		// rotated client between the receive and settlement, the deletes /
		// extensions would run under a different principal than the one that
		// received → they fail → the messages redeliver as duplicates. The
		// snapshot is passed into pollAndConvert (so the receive uses it) and
		// bound to each delivery below.
		client := r.loadClient()

		results, err := r.pollAndConvert(ctx, client, queueURL, maxMessages, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.metrics.Counter(MetricSQSPollErrors, 1,
				shared.Tag{Key: TagKeyQueueURL, Value: queueURL})

			// Classify the receive error (Finding: c8-terminal-recv). A
			// terminal/permanent fault — queue deleted, IAM revoked past the
			// auth grace, invalid URL — cannot self-heal by retrying, so
			// tight-retrying it here behind an already-closed readiness
			// signal strands a non-functional route while health reports
			// green (health lies). Surface it to the caller (superviseRoute)
			// which records the component error and restarts/degrades the
			// route in isolation. The authGrace keeps a freshly-granted role
			// retryable within its bounded propagation window; only genuinely
			// transient errors stay on the retry-with-backoff path below.
			classified := r.classify(err)
			if !shared.IsRecoverableError(classified) {
				if r.logger != nil {
					r.logger.Error("sqs: ReceiveMessage terminal fault; degrading route",
						"queue", queueURL,
						"error", classified,
					)
				}
				return classified
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
		// A successful poll authenticated: end any pending auth-failure streak
		// so a later credential-rotation gap gets a fresh grace window.
		r.authGrace.reset()

		// Create ALL deliveries for the batch BEFORE the emit loop.
		// Every batch-mate's visibility clock started ticking at
		// ReceiveMessage; emit is synchronous and can block for a long
		// time under MaxInFlight saturation, so a delivery created only
		// after the previous emit returns would burn its visibility
		// window with no auto-extend running → expiry → source
		// redelivery → duplicate amplification and a stale receipt
		// handle failing the eventual Ack. Constructing them up front
		// starts each message's auto-extend goroutine at receive time.
		pending := make([]batchDelivery, 0, len(results))
		for _, raw := range results {
			// Per-delivery context so that auto-extend failure can
			// cancel processing without affecting other deliveries.
			// The cancel func is passed into newDelivery so it is set
			// BEFORE any auto-extend goroutine starts.
			deliveryCtx, deliveryCancel := context.WithCancel(ctx)
			pending = append(pending, batchDelivery{
				del: newDelivery(
					deliveryCtx,
					raw.env,
					client,
					queueURL,
					raw.receiptHandle,
					r.cfg.VisibilityTimeout,
					r.cfg.autoExtendEnabled(),
					deliveryCancel,
					r.logger,
					r.metrics,
					r.clock(),
				),
				ctx:    deliveryCtx,
				cancel: deliveryCancel,
			})
		}

		for i, p := range pending {
			if err := emit(p.ctx, p.del); err != nil {
				// Cancel this delivery and every not-yet-emitted
				// batch-mate so their auto-extend goroutines stop;
				// the un-settled messages become visible again after
				// the current window and SQS redelivers them.
				for _, rest := range pending[i:] {
					rest.cancel()
				}
				return err
			}
		}
	}
}

// batchDelivery pairs a constructed Delivery with its per-delivery
// context so the poll loop can build a whole receive batch (starting
// auto-extend for every batch-mate) before emitting any of them.
type batchDelivery struct {
	del    ports.Delivery
	ctx    context.Context
	cancel context.CancelFunc
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

	// Clamp the jittered delay to the configured ceiling: jitter is applied
	// AFTER b.current was capped, so a +25% jitter would otherwise return up
	// to 1.25*PollBackoffMax (Finding 11). Downward jitter is preserved so
	// the backoff still de-synchronises competing pollers below the cap.
	if delay > b.max {
		delay = b.max
	}

	b.current = time.Duration(float64(b.current) * b.multiplier)
	if b.current > b.max {
		b.current = b.max
	}

	return delay
}

func (b *pollBackoff) reset() {
	b.current = b.initial
}
