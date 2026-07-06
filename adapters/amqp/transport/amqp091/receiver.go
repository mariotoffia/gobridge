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

var (
	_ ports.Receiver      = (*Receiver)(nil)
	_ ports.ContextCloser = (*Receiver)(nil)
)

// receiverChannel is the minimal AMQP channel surface the Receiver drives.
// *amqpChannel satisfies it; keeping it an interface lets the consume loop
// hand channel ownership to the receiver (for drain-then-close on graceful
// shutdown) and lets tests substitute a fake to assert that ordering
// without a live broker.
type receiverChannel interface {
	Qos(prefetchCount, prefetchSize int) error
	Consume(
		ctx context.Context,
		queue, consumerTag string,
		autoAck, exclusive bool,
		logger *slog.Logger,
		metrics ports.MetricsExporter,
		clk clock.Clock,
	) (<-chan *Delivery, error)
	NotifyClose() <-chan error
	Close() error
}

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

	// chMu guards activeCh, the channel the live consume loop handed off on
	// graceful shutdown. Close (invoked by the route runner AFTER it drains
	// in-flight deliveries) closes it so those Acks land on an open channel
	// instead of failing and forcing the broker to requeue settled work as
	// duplicates on the next start.
	chMu     sync.Mutex
	activeCh receiverChannel
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
	// raceRetries counts consecutive permanent-classified errors retried
	// as transient reconnect races (see isReconnectRaceError); it shares
	// the healthy-run reset.
	var failures int
	var raceRetries int
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

		// Reset the failure counters once a consume loop has run long
		// enough to be considered healthy, so an occasional reconnect
		// after a long stable run does not inherit an escalated backoff
		// or an exhausted race-retry budget.
		if r.clock().Since(loopStart) >= receiverHealthyRun {
			failures = 0
			raceRetries = 0
		}

		// A permanent transport error (queue/exchange missing, access
		// refused, unsupported, protocol error) recurs identically on
		// every reconnect. Failing the component surfaces the
		// misconfiguration instead of hot-looping forever on it.
		//
		// Exception: two permanent-classified codes are, in a reconnect
		// window, transient broker races — retried with a bounded budget
		// so a partition or broker restart does not turn into a route
		// crash loop. See isReconnectRaceError.
		if isPermanentError(err) {
			if !isReconnectRaceError(err, r.cfg.Exclusive) || raceRetries >= reconnectRaceRetryBudget {
				if logging.DebugEnabled(r.logger) {
					r.logger.Log(ctx, logging.LevelDebug,
						"amqp091: receiver stopping on permanent error",
						"queue", r.cfg.QueueName, "error", err)
				}
				return err
			}
			raceRetries++
			r.metrics.Counter(MetricAMQP091ReconnectRaceRetried, 1,
				shared.Tag{Key: shared.TagKeyEntity, Value: r.cfg.QueueName})
			if r.logger != nil {
				r.logger.Warn("amqp091: permanent-classified consume error retried as reconnect race",
					"queue", r.cfg.QueueName,
					"attempt", raceRetries,
					"budget", reconnectRaceRetryBudget,
					"error", err,
				)
			}
		}

		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp091: consumer channel lost, waiting for reconnect",
				"queue", r.cfg.QueueName, "error", err)
		}

		if !r.waitForReconnect(ctx) {
			// waitForReconnect returns false either because the caller
			// cancelled (ctx.Err() != nil — a clean stop) OR because the
			// session's event stream closed under us (the session was closed
			// while this route's ctx is still live). In the latter case
			// ctx.Err() is nil, so returning it would report a silent clean
			// stop: the route dies while the runtime still believes it
			// healthy. Return a non-nil transient error instead so the stop
			// is attributable and not swallowed.
			if err := ctx.Err(); err != nil {
				return err
			}
			return shared.ErrConnectionLost.WithMessage(
				"amqp091: session closed while receiver active")
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

// reconnectRaceRetryBudget bounds how many consecutive
// permanent-classified consume errors are retried as transient
// reconnect races before the receiver fails the component. With the
// receiver backoff schedule (100ms doubling, capped at 5s) ten retries
// span roughly 25-30s of wall clock — enough to outlive the two windows
// this budget exists for (a stale exclusive consumer the broker holds
// for ~2x heartbeat ≈ 20s at the 10s default, and the
// topology-re-declare window after a broker restart) while a genuine
// misconfiguration still surfaces within half a minute.
const reconnectRaceRetryBudget = 10

// isReconnectRaceError reports whether a permanent-classified consume
// error is plausibly a transient broker race around a reconnect and
// therefore worth a bounded retry instead of an immediate component
// failure (which would crash-loop the pod on the next identical race):
//
//   - 403 ACCESS_REFUSED on an EXCLUSIVE consumer: after a network
//     partition the broker still holds the previous (dead) exclusive
//     consumer until missed heartbeats (~2x heartbeat interval) reap the
//     stale connection; the re-consume is legitimately refused until then.
//     Without exclusivity a 403 is a real permission error — not retried.
//   - 404 NOT_FOUND: after a broker restart a non-durable queue is gone
//     until the session's reconcile re-declares it; a consume that races
//     that window sees NOT_FOUND even though the topology heals moments
//     later.
func isReconnectRaceError(err error, exclusive bool) bool {
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		return false
	}
	switch be.Code {
	case shared.ErrCodeNotFound:
		return true
	case shared.ErrCodeNotAuthorized:
		return exclusive
	default:
		return false
	}
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
	return r.runChannel(ctx, ch, emit)
}

// runChannel drives one consume attempt on ch. It takes ownership of the
// channel for the lifetime of the attempt:
//
//   - On any error/reconnect return it closes the channel so the next
//     attempt (Run's loop) opens a fresh one.
//   - On a graceful stop (ctx cancelled) it HANDS the channel off to
//     Receiver.Close instead of closing it here. The route runner drains
//     in-flight deliveries and only then calls Close, so those deliveries'
//     Acks settle on a still-open channel. Closing here (the previous
//     `defer ch.Close()`) tore the channel down while up to prefetch_count
//     deliveries were mid-pipeline, failing their Acks and forcing the
//     broker to requeue settled work as duplicates on every shutdown.
//
// Because a graceful stop hands the channel off rather than closing it,
// callers driving Run directly MUST call Receiver.Close after Run returns,
// or the channel and its forwarder goroutine leak.
func (r *Receiver) runChannel(ctx context.Context, ch receiverChannel, emit func(context.Context, ports.Delivery) error) error {
	r.setActiveChannel(ch)
	handOff := false
	defer func() {
		if !handOff {
			r.closeActiveChannel(ch)
		}
	}()

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

	// Derive a per-attempt context so that when this consume attempt returns
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
			handOff = true
			return nil
		default:
		}
		select {
		case <-ctx.Done():
			handOff = true
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

// setActiveChannel records ch as the channel the current consume attempt
// owns, so Receiver.Close can settle in-flight deliveries before tearing
// it down on graceful shutdown.
func (r *Receiver) setActiveChannel(ch receiverChannel) {
	r.chMu.Lock()
	r.activeCh = ch
	r.chMu.Unlock()
}

// closeActiveChannel closes ch and clears the active reference if it still
// points at ch. Called on every error/reconnect return from runChannel so
// the next consume attempt opens a fresh channel.
func (r *Receiver) closeActiveChannel(ch receiverChannel) {
	r.chMu.Lock()
	if r.activeCh == ch {
		r.activeCh = nil
	}
	r.chMu.Unlock()
	_ = ch.Close()
}

// Close releases the consumer channel handed off on graceful shutdown.
// The route runner (runtime/route/runner.go) invokes it AFTER draining
// in-flight deliveries, so their Acks land on the still-open channel and
// the broker does not requeue settled work as duplicates on the next
// start. Safe to call when no channel is outstanding (nil activeCh).
//
// Close honours ctx (ports.ContextCloser): the SDK's Channel.Close issues a
// channel.close RPC and BLOCKS on close-ok, which on a half-dead/unresponsive
// connection only resolves via missed-heartbeat detection (~2× heartbeat).
// The runner caps teardown with a bounded ctx, so the close is detached onto
// a background goroutine and raced against ctx.Done: when ctx expires Close
// returns promptly while the goroutine still completes the channel.Close (the
// broker requeues any unacked deliveries on that channel — at-least-once).
//
// Embedders driving Run directly MUST call Close after Run returns on a
// graceful stop; otherwise the handed-off consumer channel and its forwarder
// goroutine leak (Run no longer self-closes the channel — see runChannel).
func (r *Receiver) Close(ctx context.Context) error {
	r.chMu.Lock()
	ch := r.activeCh
	r.activeCh = nil
	r.chMu.Unlock()
	if ch == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- ch.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// The detached goroutine still completes ch.Close(); the broker
		// requeues any unacked deliveries on that channel.
		return ctx.Err()
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
