package servicebus

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
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
//
// client / scheduler / asbClient form the swappable receiver stack:
// every access goes through initMu (currentClient / currentScheduler /
// swapStack) so a live credential rotation (ApplyCredentials) can
// replace the whole stack while the poll loop is running — the loop
// only ever observes either the complete old stack or the complete new
// one, never nil.
//
// rebuildPending / pendingConn track a deferred session-mode rebuild:
// a session rotation must close the old link before the new accept can
// win the exclusive session lock, so if that rebuild then fails the
// stack is left empty and rebuildPending is set with the (uncommitted)
// target connection. cfg.Connection is NOT advanced until a rebuild
// succeeds, so re-pushing the SAME credentials is never short-circuited
// to a no-op; the poll loop (rebuildPendingStack) retries the build
// with pendingConn until it succeeds, so the receiver self-heals
// without an external re-push.
type Receiver struct {
	cfg       ReceiverConfig
	client    asbAPI
	scheduler retryScheduler
	asbClient *asbClientHandle
	// pendingConn holds the connection a deferred session-mode rebuild
	// must use; rebuildPending reports whether such a rebuild is
	// outstanding. Both are guarded by initMu.
	pendingConn    ConnectionConfig
	rebuildPending bool
	// buildStackFn overrides buildStack for tests that need to simulate
	// build failures/successes deterministically; nil in production.
	buildStackFn func(context.Context, ConnectionConfig) (receiverStack, error)
	logger       *slog.Logger
	metrics      ports.MetricsExporter
	clk          clock.Clock
	initMu       sync.Mutex
	closeOnce    sync.Once
	started      chan struct{}
	startedOnce  sync.Once
}

// receiverStack bundles the SDK client handle with the receiver and
// scheduler seams built from it, so init/rotation/close can move all
// three as one unit.
type receiverStack struct {
	client    asbAPI
	scheduler retryScheduler
	handle    *asbClientHandle
}

// close releases the stack's resources; nil members are skipped.
func (s receiverStack) close(ctx context.Context) {
	if closer, ok := s.client.(ports.ContextCloser); ok {
		_ = closer.Close(ctx)
	}
	if s.scheduler != nil {
		if closer, ok := s.scheduler.(ports.ContextCloser); ok {
			_ = closer.Close(ctx)
		}
	}
	if s.handle != nil {
		_ = s.handle.Close(ctx)
	}
}

func NewReceiver(cfg ReceiverConfig, logger *slog.Logger) (*Receiver, error) {
	requestedMaxMessages := cfg.MaxMessages
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
	if cfg.receiveAndDelete() && requestedMaxMessages > 1 && l != nil {
		// applyDefaults clamps MaxMessages to 1 in ReceiveAndDelete mode:
		// the broker deletes at receive time, so every batched-but-not-
		// yet-emitted message would be lost on shutdown or emit error.
		l.Warn("servicebus: receive_mode ReceiveAndDelete forces max_messages=1 (broker settles at receive; larger batches risk message loss on shutdown)",
			"requested_max_messages", requestedMaxMessages)
	}
	return &Receiver{cfg: cfg, logger: l, metrics: m, clk: clk, started: make(chan struct{})}, nil
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// currentClient returns the live receiver seam under the stack lock.
// Nil means "not started" (or closed).
func (r *Receiver) currentClient() asbAPI {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	return r.client
}

// currentScheduler returns the live retry scheduler under the stack
// lock. Nil is valid (topic subscription, or sender build failed).
//
//nolint:ireturn // retryScheduler is an adapter-internal mock seam (category 5).
func (r *Receiver) currentScheduler() retryScheduler {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	return r.scheduler
}

// swapStack installs next as the live stack under the stack lock and
// returns the previous stack so the caller can close it OUTSIDE the
// lock (Close on AMQP links can block).
func (r *Receiver) swapStack(next receiverStack) receiverStack {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	old := receiverStack{client: r.client, scheduler: r.scheduler, handle: r.asbClient}
	r.client = next.client
	r.scheduler = next.scheduler
	r.asbClient = next.handle
	return old
}

// commitStack installs next as the live stack, commits conn as the
// active connection, and clears any pending-rebuild state — all under
// the stack lock. It is the success path for both cold init and
// credential rotation: cfg.Connection only ever advances here, AFTER a
// successful build, mirroring Sender.swapClient. Returns the previous
// stack so the caller can close it OUTSIDE the lock.
func (r *Receiver) commitStack(next receiverStack, conn ConnectionConfig) receiverStack {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	old := receiverStack{client: r.client, scheduler: r.scheduler, handle: r.asbClient}
	r.client = next.client
	r.scheduler = next.scheduler
	r.asbClient = next.handle
	r.cfg.Connection = conn
	r.rebuildPending = false
	r.pendingConn = ConnectionConfig{}
	return old
}

// beginSessionRebuild performs the close-before-build step of a
// session-mode rotation: it swaps in an EMPTY stack, records conn as
// the pending rebuild target WITHOUT committing cfg.Connection, and
// marks rebuildPending so a later re-push of the same credentials is
// not short-circuited and the poll loop can retry the build. Returns
// the previous stack for the caller to close OUTSIDE the lock.
func (r *Receiver) beginSessionRebuild(conn ConnectionConfig) receiverStack {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	old := receiverStack{client: r.client, scheduler: r.scheduler, handle: r.asbClient}
	r.client = nil
	r.scheduler = nil
	r.asbClient = nil
	r.rebuildPending = true
	r.pendingConn = conn
	return old
}

// build constructs a receiver stack from conn, honouring the optional
// buildStackFn test seam. Production builds via buildStack.
func (r *Receiver) build(ctx context.Context, conn ConnectionConfig) (receiverStack, error) {
	if r.buildStackFn != nil {
		return r.buildStackFn(ctx, conn)
	}
	return r.buildStack(ctx, conn)
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
		old := r.swapStack(receiverStack{})
		old.close(ctx)
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

	stack, err := r.build(ctx, r.cfg.Connection)
	if err != nil {
		return err
	}

	r.client = stack.client
	r.scheduler = stack.scheduler
	r.asbClient = stack.handle

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "servicebus: client initialized",
			"entity", r.entityName(),
			"session_id", r.cfg.SessionID,
		)
	}

	return nil
}

// buildStack constructs a complete receiver stack (client handle,
// receiver seam, retry scheduler) from conn. Shared by ensureClient
// (first Run) and ApplyCredentials (live rotation) so both paths have
// identical semantics. It never mutates the Receiver; the caller
// installs the stack under initMu.
func (r *Receiver) buildStack(ctx context.Context, conn ConnectionConfig) (receiverStack, error) {
	asbClient, err := buildClient(conn)
	if err != nil {
		return receiverStack{}, err
	}

	recvOpts := asbReceiverOptions{
		ReceiveAndDelete: r.cfg.receiveAndDelete(),
		SubQueue:         r.cfg.SubQueue,
	}
	entityName := r.entityName()

	stack := receiverStack{handle: asbClient}

	if r.cfg.SessionID != "" {
		sessOpts := asbSessionOptions{
			ReceiveAndDelete: r.cfg.receiveAndDelete(),
		}

		// Session accept DIALS the broker and competes for the session
		// lock. During rolling deploys the outgoing pod still holds it
		// (com.microsoft:session-cannot-be-locked), which is expected and
		// transient — retry with backoff instead of crash-looping the
		// whole bridge on a one-shot accept.
		accept := func(ctx context.Context) (asbAPI, error) {
			if r.cfg.QueueName != "" {
				return asbClient.AcceptSessionForQueue(ctx, r.cfg.QueueName, r.cfg.SessionID, sessOpts)
			}
			return asbClient.AcceptSessionForSubscription(ctx, r.cfg.TopicName, r.cfg.SubscriptionName, r.cfg.SessionID, sessOpts)
		}
		seam, err := acceptSessionWithRetry(ctx, accept, r.clock(), r.logger)
		if err != nil {
			_ = asbClient.Close(context.Background())
			return receiverStack{}, shared.ErrUnavailable.Wrap(err)
		}
		stack.client = seam
	} else {
		var seam asbAPI
		if r.cfg.QueueName != "" {
			seam, err = asbClient.NewReceiverForQueue(r.cfg.QueueName, recvOpts)
		} else {
			seam, err = asbClient.NewReceiverForSubscription(r.cfg.TopicName, r.cfg.SubscriptionName, recvOpts)
		}
		if err != nil {
			_ = asbClient.Close(context.Background())
			return receiverStack{}, shared.ErrUnavailable.Wrap(err)
		}
		stack.client = seam
	}

	// Finding 2 (no message duplication): the scheduled-retry sender
	// addresses the entity by name. For a topic subscription the entity
	// name is the TOPIC, so a scheduled retry would be published to the
	// topic and fan out to *every* sibling subscription — duplicating the
	// message. Only a queue can safely self-schedule (its entity name is
	// the queue itself). For subscriptions we leave scheduler == nil and
	// asbDelivery.Retry falls back to AbandonMessage (same-subscription
	// redelivery, no fan-out).
	if r.cfg.QueueName != "" {
		sender, err := asbClient.NewSender(entityName)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("servicebus: could not create retry scheduler sender",
					"entity", entityName, "error", err)
			}
		} else {
			stack.scheduler = sender
		}
	} else if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug,
			"servicebus: delayed-retry scheduler disabled for topic subscription (would fan out to sibling subscriptions); using abandon-based redelivery",
			"topic", r.cfg.TopicName,
			"subscription", r.cfg.SubscriptionName,
		)
	}

	return stack, nil
}

// sessionAcceptMaxAttempts bounds acceptSessionWithRetry. With the
// pollBackoff schedule (1s..30s, x2) five attempts span roughly 15s of
// backoff — enough to ride out a rolling-deploy lock handover without
// masking a genuinely dead entity.
const sessionAcceptMaxAttempts = 5

// acceptSessionWithRetry retries a session accept with exponential
// backoff. Retryable: com.microsoft:session-cannot-be-locked (the old
// holder's lock has not lapsed yet — expected during rolling deploys),
// no-session-available timeouts, and everything MapError classifies as
// recoverable. Permanent errors (unauthorized, entity not found, ...)
// fail immediately.
func acceptSessionWithRetry(
	ctx context.Context,
	accept func(context.Context) (asbAPI, error),
	clk clock.Clock,
	logger *slog.Logger,
) (asbAPI, error) {
	backoff := newPollBackoff()
	var lastErr error
	for attempt := 1; ; attempt++ {
		seam, err := accept(ctx)
		if err == nil {
			return seam, nil
		}
		lastErr = err
		if !isRetryableSessionAcceptError(err) || attempt >= sessionAcceptMaxAttempts {
			return nil, lastErr
		}
		delay := backoff.next()
		if logger != nil {
			logger.Warn("servicebus: session accept failed, retrying",
				"attempt", attempt,
				"max_attempts", sessionAcceptMaxAttempts,
				"retry_after", delay,
				"error", err,
			)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clk.After(delay):
		}
	}
}

// isRetryableSessionAcceptError reports whether a session accept
// failure is worth retrying. session-cannot-be-locked never gets its
// own azservicebus.Code (the SDK treats it as fatal for the ONE
// accept), so it is matched on the AMQP error text.
func isRetryableSessionAcceptError(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(strings.ToLower(err.Error()), "session-cannot-be-locked") {
		return true
	}
	return shared.IsRecoverableError(MapError(err))
}

// isSessionRequiredError matches the amqp:not-allowed failure a
// NON-session receiver gets on a session-enabled entity ("it is not
// possible for an entity that requires sessions to create a
// non-sessionful message receiver"). Retrying can never succeed, so
// the poll loop fails fast instead of warn-looping forever.
func isSessionRequiredError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "requires sessions") ||
		strings.Contains(msg, "sessionful")
}

func (r *Receiver) pollLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	backoff := newPollBackoff()

	r.startedOnce.Do(func() { close(r.started) })

	// Auto-extend never runs in ReceiveAndDelete mode: the broker settles
	// the message at receive time, so there is no lock to renew.
	autoExtend := r.cfg.autoExtendEnabled() && !r.cfg.receiveAndDelete()
	// A topic subscription cannot self-schedule a delayed retry without
	// fanning the scheduled copy out to sibling subscriptions, so the
	// delivery falls back to abandon (logged at debug, not error).
	delayedRetryDisabled := r.cfg.QueueName == ""
	receiveAndDelete := r.cfg.receiveAndDelete()

	sessionMode := r.cfg.SessionID != ""
	if sessionMode && autoExtend {
		// In session mode every in-flight delivery shares ONE session
		// lock (sessionReceiverAdapter.RenewMessageLock renews the
		// session, not the message). Per-delivery auto-extend would spawn
		// up to MaxMessages goroutines all renewing the same lock, so a
		// single session-renewer goroutine replaces them for the life of
		// the poll loop. It has no MaxLockRenewalDuration cap: the
		// session lock must be held as long as the loop runs; the cap is
		// a per-delivery hung-pipeline guard, which does not translate to
		// session scope.
		renewCtx, renewCancel := context.WithCancel(ctx)
		defer renewCancel()

		interval := r.cfg.LockDuration / 2
		if floor := r.cfg.MinAutoExtendInterval; floor > 0 && interval < floor {
			interval = floor
		}
		go r.runSessionRenewer(renewCtx, interval)
	}

	tuning := deliveryTuning{
		lockDuration:          r.cfg.LockDuration,
		autoExtend:            autoExtend && !sessionMode,
		minAutoExtendInterval: r.cfg.MinAutoExtendInterval,
		maxLockRenewal:        r.cfg.MaxLockRenewalDuration,
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Session-mode rotation closes the old link before rebuilding
		// (exclusive session lock). If a prior ApplyCredentials left that
		// rebuild pending (its build failed), retry it here with the
		// pending connection so the receiver self-heals without an
		// external re-push. A failed retry is treated like a poll failure:
		// count it, back off, and try again.
		if sessionMode {
			if err := r.rebuildPendingStack(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.metrics.Counter(MetricASBReceiveFailures, 1,
					shared.Tag{Key: shared.TagKeyEntity, Value: r.entityName()})
				delay := backoff.next()
				if r.logger != nil {
					r.logger.Warn("servicebus: pending session rebuild failed, retrying",
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
		}

		pollStart := r.clock().Now()
		raws, err := r.pollAndConvert(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !sessionMode && isSessionRequiredError(err) {
				// A non-session receiver on a session-enabled entity can
				// never succeed: fail fast with a clear remedy instead of
				// warn-looping forever. AcceptNextSession-style rotating
				// session consumption is not implemented.
				return shared.ErrNotSupported.Wrap(fmt.Errorf(
					"servicebus: entity %q requires sessions but no session_id is configured; set receiver.session_id (accept-next-session polling is not supported): %w",
					r.entityName(), err))
			}
			r.metrics.Counter(MetricASBReceiveFailures, 1,
				shared.Tag{Key: shared.TagKeyEntity, Value: r.entityName()})
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

		scheduler := r.currentScheduler()

		// Build ALL deliveries first so lock auto-renewal starts for the
		// whole batch immediately after receive. emit() below can block on
		// pipeline backpressure; if renewal only started per message right
		// before its own emit, the locks of the batch tail would lapse
		// unrenewed — redelivery duplicates plus LockLost on settle.
		type pendingDelivery struct {
			del    *asbDelivery
			ctx    context.Context
			cancel context.CancelFunc
		}
		pending := make([]pendingDelivery, 0, len(raws))
		for _, raw := range raws {
			// Per-message context tree (mirrors the SQS adapter):
			// deliveryCtx is handed to emit() AND to newDelivery (as
			// processingCancel). If emit fails we cancel it so the
			// auto-extend goroutine stops and the broker lock lapses for
			// redelivery — never left silently held. On the happy path the
			// settlement methods free it via cleanupContext().
			deliveryCtx, deliveryCancel := context.WithCancel(ctx)
			del := newDelivery(
				deliveryCtx,
				raw.env,
				raw.client,
				scheduler,
				raw.msg,
				tuning,
				deliveryCancel,
				r.logger,
				r.metrics,
				r.clock(),
			)
			del.receiveAndDelete = receiveAndDelete
			del.delayedRetryDisabled = delayedRetryDisabled
			pending = append(pending, pendingDelivery{del: del, ctx: deliveryCtx, cancel: deliveryCancel})
		}

		for i, pd := range pending {
			if err := emit(pd.ctx, pd.del); err != nil {
				// Cancel this delivery and every not-yet-emitted one so
				// their auto-extend goroutines stop and the broker locks
				// lapse for redelivery.
				for _, rest := range pending[i:] {
					rest.cancel()
				}
				return err
			}
		}
	}
}

// rebuildPendingStack completes a deferred session-mode rebuild: when a
// prior rotation closed the old stack (beginSessionRebuild) but the
// build failed, it retries the build with the pending connection and
// commits it on success. It is a no-op when nothing is pending. Only
// invoked from the session poll loop, so at most one rebuild runs from
// here at a time; a concurrent ApplyCredentials re-push is safe — the
// loser's freshly built stack is returned as the "old" stack and
// closed.
func (r *Receiver) rebuildPendingStack(ctx context.Context) error {
	r.initMu.Lock()
	if !r.rebuildPending {
		r.initMu.Unlock()
		return nil
	}
	conn := r.pendingConn
	r.initMu.Unlock()

	stack, err := r.build(ctx, conn)
	if err != nil {
		return fmt.Errorf("servicebus: retry pending session rebuild for %q: %w", r.entityName(), err)
	}

	old := r.commitStack(stack, conn)
	old.close(ctx)

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug,
			"servicebus: pending session rebuild completed",
			"entity", r.entityName(),
			"session_id", r.cfg.SessionID,
		)
	}
	return nil
}

// runSessionRenewer renews the shared session lock at interval until
// ctx is done. One instance per poll loop in session mode; replaces
// the per-delivery auto-extend goroutines (which would all renew the
// SAME session lock redundantly). Tolerates up to autoExtendMaxFailures
// consecutive errors, then stops: the session lock will lapse and the
// subsequent ReceiveMessages failures surface through the poll loop's
// error path (backoff + MetricASBReceiveFailures).
//
// It resolves the live receiver seam via currentClient() on EVERY tick
// rather than pinning a snapshot: a credential rotation swaps the stack
// under the still-running poll loop, and renewing the OLD (closed)
// client's session would leave the NEW session's lock unrenewed
// (lock-lost on in-flight deliveries). A nil snapshot — the brief
// window while a session rebuild is pending — is skipped without
// counting as a failure; the next tick renews the rebuilt session.
func (r *Receiver) runSessionRenewer(ctx context.Context, interval time.Duration) {
	ticker := r.clock().NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			client := r.currentClient()
			if client == nil {
				// Stack swap in progress (session rebuild pending): the
				// rebuilt session's lock is renewed on a later tick.
				continue
			}
			// The message argument is unused by sessionReceiverAdapter:
			// session receivers renew the session lock, not a message lock.
			if err := client.RenewMessageLock(ctx, nil, nil); err != nil {
				if ctx.Err() != nil {
					return
				}
				consecutiveFailures++
				if r.logger != nil {
					r.logger.Warn("servicebus: session lock renewal failed",
						"session_id", r.cfg.SessionID,
						"error", err,
						"consecutive_failures", consecutiveFailures,
					)
				}
				if consecutiveFailures >= autoExtendMaxFailures {
					if r.logger != nil {
						r.logger.Error("servicebus: session lock renewal max failures reached, stopping renewer",
							"session_id", r.cfg.SessionID,
						)
					}
					return
				}
				continue
			}
			consecutiveFailures = 0
			r.metrics.Counter(MetricASBLockRenewals, 1)
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
