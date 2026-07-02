package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for AMQP 0-9-1. It consumes
// messages from a queue via the Session's connection and emits
// each message as a ports.Delivery.
type Receiver struct {
	cfg         ReceiverConfig
	session     *Session
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	started     chan struct{}
	startedOnce sync.Once
}

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	clk := cfg.Clock
	if clk == nil && cfg.Session != nil {
		clk = cfg.Session.clk
	}
	if clk == nil {
		clk = clock.System
	}
	l := cfg.Logger
	if l == nil && cfg.Session != nil {
		l = cfg.Session.logger
	}
	return &Receiver{
		cfg:     cfg,
		session: cfg.Session,
		logger:  l,
		metrics: m,
		clk:     clk,
		started: make(chan struct{}),
	}
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the receiver's
// channel and consumer have been set up and the consume loop is live.
// It satisfies ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// Run starts consuming messages from the configured queue. It blocks
// until ctx is cancelled or an unrecoverable error occurs. On channel
// or connection errors, it waits for the session to reconnect and
// re-establishes the consumer.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "amqp091: receiver starting",
			"queue", r.cfg.QueueName,
			"consumer_tag", r.cfg.ConsumerTag,
		)
	}

	// failures counts consecutive rapid consume failures to drive the
	// reconnect backoff; it resets after a healthy run (see loop body).
	var failures int
	for {
		loopStart := r.clock().Now()
		err := r.consumeLoop(ctx, emit)
		if err == nil || ctx.Err() != nil {
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "amqp091: receiver stopped",
					"queue", r.cfg.QueueName, "reason", "context_cancelled")
			}
			return ctx.Err()
		}

		if r.isEmitError(err) {
			return errors.Unwrap(err)
		}

		// A permanent transport error (queue/exchange missing, access
		// refused, unsupported, protocol error) recurs identically on
		// every reconnect. Failing the component surfaces the
		// misconfiguration instead of hot-looping forever on it.
		if isPermanentError(err) {
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug,
					"amqp091: receiver stopping on permanent error",
					"queue", r.cfg.QueueName, "error", err)
			}
			return err
		}

		// Reset the failure counter once a consume loop has run long
		// enough to be considered healthy, so an occasional reconnect
		// after a long stable run does not inherit an escalated backoff.
		if r.clock().Since(loopStart) >= receiverHealthyRun {
			failures = 0
		}

		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp091: consumer channel lost, waiting for reconnect",
				"queue", r.cfg.QueueName, "error", err)
		}

		if !r.waitForReconnect(ctx) {
			return ctx.Err()
		}

		// Bounded backoff before re-establishing the consumer. When the
		// session stays connected but consumeLoop keeps failing fast
		// (e.g. the broker repeatedly cancels the consumer, or a transient
		// channel error), waitForReconnect returns immediately because
		// Health reports Connected. Without this delay the loop hot-spins,
		// burning CPU and hammering the broker. The delay grows with
		// consecutive failures and is reset after a healthy run above.
		failures++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.clock().After(receiverBackoff(failures)):
		}
	}
}

// emitError wraps errors from the emit callback so they can be
// distinguished from transport-layer errors.
type emitError struct{ err error }

func (e *emitError) Error() string { return e.err.Error() }
func (e *emitError) Unwrap() error { return e.err }

const (
	// receiverRetryInitial is the first backoff applied after a transient
	// consume failure that left the session connected.
	receiverRetryInitial = 100 * time.Millisecond
	// receiverRetryMax caps the exponential reconnect backoff.
	receiverRetryMax = 5 * time.Second
	// receiverHealthyRun is how long a consume loop must run before a
	// subsequent failure is treated as fresh and the backoff resets.
	receiverHealthyRun = 30 * time.Second
)

// isPermanentError reports whether err is a classified permanent
// transport error (queue/exchange missing, access refused, unsupported,
// protocol error). Such errors recur identically on every reconnect, so
// the receiver fails the component instead of retrying forever.
func isPermanentError(err error) bool {
	var be *shared.BridgeError
	if errors.As(err, &be) {
		return be.Class == shared.ErrorPermanent
	}
	return false
}

// receiverBackoff returns the bounded exponential backoff for the nth
// consecutive rapid failure (failures >= 1).
func receiverBackoff(failures int) time.Duration {
	if failures <= 1 {
		return receiverRetryInitial
	}
	shift := failures - 1
	if shift > 8 { // 100ms<<8 already exceeds the cap; avoid shift overflow
		shift = 8
	}
	d := receiverRetryInitial << uint(shift)
	if d <= 0 || d > receiverRetryMax {
		return receiverRetryMax
	}
	return d
}

// isEmitError returns true when the error originated from the emit callback
// rather than from the AMQP transport layer.
func (r *Receiver) isEmitError(err error) bool {
	var ee *emitError
	return errors.As(err, &ee)
}

func (r *Receiver) consumeLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	ch, err := r.openChannel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	if r.cfg.PrefetchCount > 0 || r.cfg.PrefetchSize > 0 {
		if err := ch.Qos(r.cfg.PrefetchCount, r.cfg.PrefetchSize); err != nil {
			return MapError(err)
		}
	}

	// Generate a fresh tag per consume attempt. Even when the user
	// supplies a base tag, append a unique suffix so that reconnecting
	// after a connection drop never collides with the broker's stale
	// view of the previous consumer (RabbitMQ only frees a tag once it
	// detects the prior connection is dead, which can lag the client).
	consumerTag := generateConsumerTag()
	if r.cfg.ConsumerTag != "" {
		consumerTag = r.cfg.ConsumerTag + "-" + consumerTag
	}

	// Derive a per-attempt context so that when this consumeLoop returns
	// for ANY reason (broker channel close, delivery-channel close, a
	// consume error, or caller cancellation) the forwarding goroutine
	// started inside ch.Consume is released from a blocked send on its
	// out channel and nack-requeues the in-flight delivery instead of
	// leaking. The parent ctx is long-lived (reused across reconnects in
	// Run) and only cancels on full receiver shutdown, so without this a
	// broker-initiated channel close would strand the forwarder forever
	// (one leaked goroutine + one unsettled delivery per channel flap).
	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	consumeStart := r.clock().Now()
	deliveries, err := ch.Consume(
		consumeCtx,
		r.cfg.QueueName,
		consumerTag,
		r.cfg.AutoAck,
		r.cfg.Exclusive,
		r.logger,
		r.metrics,
		r.clock(),
	)
	if err != nil {
		return MapError(err)
	}
	r.metrics.Timer(MetricAMQP091ConsumeLatency, r.clock().Since(consumeStart),
		shared.Tag{Key: shared.TagKeyEntity, Value: r.cfg.QueueName})

	r.startedOnce.Do(func() { close(r.started) })

	chanClose := ch.NotifyClose()

	for {
		// Priority check: if the caller has cancelled the context, return
		// before the normal multi-way select picks a delivery or
		// channel-close event randomly. Without this, the runtime's
		// fair scheduling of select cases would let the loop process
		// pending deliveries even after cancellation, defeating
		// graceful shutdown under load.
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case chanErr, ok := <-chanClose:
			if ok && chanErr != nil {
				return MapError(chanErr)
			}
			return shared.ErrConnectionLost.WithMessage("amqp091: channel closed by broker")
		case d, ok := <-deliveries:
			if !ok {
				return shared.ErrConnectionLost.WithMessage("amqp091: delivery channel closed")
			}
			if err := r.handleDelivery(ctx, d, emit); err != nil {
				return err
			}
		}
	}
}

func (r *Receiver) handleDelivery(ctx context.Context, d *Delivery, emit func(context.Context, ports.Delivery) error) error {
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "amqp091: message received",
			"queue", r.cfg.QueueName,
			"payload_len", len(d.Envelope().Payload()),
		)
	}

	if err := emit(ctx, d); err != nil {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp091: emit error",
				"queue", r.cfg.QueueName, "error", err)
		}
		return &emitError{err: err}
	}
	return nil
}

func (r *Receiver) openChannel() (*amqpChannel, error) {
	if r.session == nil {
		return nil, shared.ErrUnavailable.WithMessage("amqp091: no session")
	}
	conn := r.session.Connection()
	if conn == nil {
		return nil, shared.ErrUnavailable.WithMessage("amqp091: session not connected")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, MapError(err)
	}
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace,
			"amqp091: receiver channel opened",
			"queue", r.cfg.QueueName,
		)
	}
	return ch, nil
}

func (r *Receiver) waitForReconnect(ctx context.Context) bool {
	if r.session == nil {
		return false
	}
	// Use Subscribe so multiple receivers (and any user-side observer
	// reading from Events()) all receive the SessionConnected event
	// independently. Reading from Events() directly would steal the
	// notification from siblings and cause them to hang forever.
	events, unsub := r.session.Subscribe()
	defer unsub()

	// Race window: between the receiver's consumeLoop returning (channel
	// lost) and reaching this point, the session may have ALREADY
	// reconnected and emitted SessionConnected. That event was delivered
	// to whatever subscribers existed at the time and is gone — we
	// subscribed too late. Probe the current health up front so we
	// proceed immediately when the session is already healthy, instead
	// of hanging until ctx expires.
	if h := r.session.Health(ctx); h.Connected {
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(context.Background(), logging.LevelTrace,
				"amqp091: receiver reconnect probe found session already connected",
				"queue", r.cfg.QueueName,
			)
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if ev.Type == ports.SessionConnected || ev.Type == ports.SessionReconciled {
				if logging.TraceEnabled(r.logger) {
					r.logger.Log(context.Background(), logging.LevelTrace,
						"amqp091: receiver reconnect signal received",
						"queue", r.cfg.QueueName,
						"event_type", ev.Type,
					)
				}
				return true
			}
		}
	}
}
