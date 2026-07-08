package servicebus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
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
	// outstanding. rebuildGen is bumped by beginSessionRebuild to fence
	// stale builds: a build that started for an earlier generation must
	// not clobber a newer committed stack (last-writer-wins hazard when a
	// slow rebuild races a newer rotation). All three are guarded by
	// initMu.
	pendingConn    ConnectionConfig
	rebuildPending bool
	rebuildGen     uint64
	// buildStackFn overrides buildStack for tests that need to simulate
	// build failures/successes deterministically; nil in production.
	buildStackFn func(context.Context, ConnectionConfig) (receiverStack, error)
	// acceptNextFn overrides the accept-next-session dial for tests
	// (use_sessions mode); nil in production, where ensureSessionSeam
	// dials via the live asbClient handle. Guarded by initMu.
	acceptNextFn func(context.Context) (asbAPI, error)
	// inFlightDeliveries counts built-but-unsettled deliveries in
	// use_sessions mode. Idle session rotation (releaseSessionSeam) is
	// deferred while it is non-zero: the outstanding deliveries settle
	// against the current seam, and closing it under them would fail
	// their Ack/Retry and force redelivery churn.
	inFlightDeliveries atomic.Int64
	// sessionGen is bumped every time ensureSessionSeam installs a newly
	// accepted session seam (use_sessions rotation). The single
	// long-lived session renewer reads it to reset its consecutive-
	// failure budget per accepted session, so a blip on one session never
	// denies renewal to the next one it accepts (F3). Lock-free on read.
	sessionGen  atomic.Uint64
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	initMu      sync.Mutex
	closeOnce   sync.Once
	started     chan struct{}
	startedOnce sync.Once
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
// not short-circuited and the poll loop can retry the build. It bumps
// rebuildGen and returns the new generation so the eventual commit can
// detect a newer rotation that superseded it. Returns the previous
// stack for the caller to close OUTSIDE the lock.
func (r *Receiver) beginSessionRebuild(conn ConnectionConfig) (receiverStack, uint64) {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	old := receiverStack{client: r.client, scheduler: r.scheduler, handle: r.asbClient}
	r.client = nil
	r.scheduler = nil
	r.asbClient = nil
	r.rebuildPending = true
	r.pendingConn = conn
	r.rebuildGen++
	return old, r.rebuildGen
}

// commitRebuild installs next and commits conn IFF the rebuild
// generation is still gen — i.e. no newer beginSessionRebuild
// superseded this build while it ran. This fences the last-writer-wins
// hazard where a slow rebuild of an OLDER connection completes after a
// newer rotation already committed, which would otherwise overwrite the
// newer stack and roll cfg.Connection back to stale credentials.
//
// Because a generation maps 1:1 to a single beginSessionRebuild(conn)
// (both set under the same lock), every builder that captured gen G
// targets the SAME conn, so a same-generation double-commit is benign
// (converges to conn, the redundant stack is closed).
//
// Returns (stackToClose, applied): when applied, stackToClose is the
// previous live stack; when superseded, stackToClose is next itself, so
// the caller closes the discarded build. Either way the caller closes
// the returned stack OUTSIDE the lock.
func (r *Receiver) commitRebuild(gen uint64, next receiverStack, conn ConnectionConfig) (receiverStack, bool) {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	if r.rebuildGen != gen {
		// A newer rotation superseded this build; discard it.
		return next, false
	}
	old := receiverStack{client: r.client, scheduler: r.scheduler, handle: r.asbClient}
	r.client = next.client
	r.scheduler = next.scheduler
	r.asbClient = next.handle
	r.cfg.Connection = conn
	r.rebuildPending = false
	r.pendingConn = ConnectionConfig{}
	return old, true
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

	// A live handle with a nil client seam is the normal idle state of a
	// use_sessions receiver (no session currently held), so the handle —
	// not the seam — is the "already initialised" signal.
	if r.client != nil || r.asbClient != nil {
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

	switch {
	case r.cfg.UseSessions:
		// use_sessions: no receiver seam is built here — holding no
		// session is this mode's normal idle state. The poll loop accepts
		// the next available session lazily (ensureSessionSeam) and
		// rotates to another session when the current one idles. A cold
		// Run on an idle entity must not block waiting for a session to
		// appear, so the build succeeds with a nil seam.
	case r.cfg.SessionID != "":
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
	default:
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

	pinnedSession := r.cfg.SessionID != ""
	useSessions := r.cfg.UseSessions
	sessionMode := pinnedSession || useSessions
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
		// count it, back off, and try again. Only a PINNED session rotates
		// close-before-build; use_sessions rotates build-first like a
		// non-session receiver (no exclusive lock held at handle level).
		if pinnedSession {
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

		// use_sessions: make sure a session is held before polling. "No
		// session available right now" (SDK CodeTimeout) is an idle
		// entity, not a failure — back off quietly without counting a
		// receive failure.
		if useSessions {
			if err := r.ensureSessionSeam(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				delay := backoff.next()
				if errors.Is(MapError(err), shared.ErrTimeout) {
					if logging.DebugEnabled(r.logger) {
						r.logger.Log(ctx, logging.LevelDebug,
							"servicebus: no session available, retrying",
							"entity", r.entityName(),
							"retry_after", delay,
						)
					}
				} else {
					r.metrics.Counter(MetricASBReceiveFailures, 1,
						shared.Tag{Key: shared.TagKeyEntity, Value: r.entityName()})
					if r.logger != nil {
						r.logger.Warn("servicebus: accept next session failed, retrying",
							"entity", r.entityName(),
							"error", err,
							"retry_after", delay,
						)
					}
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
				// warn-looping forever.
				return shared.ErrNotSupported.Wrap(fmt.Errorf(
					"servicebus: entity %q requires sessions but the receiver is not session-aware; set receiver.session_id to pin one session or receiver.use_sessions to rotate over available sessions: %w",
					r.entityName(), err))
			}
			if useSessions {
				// The session link is suspect (session lock lost, link
				// detached): shed it so the next iteration accepts a fresh
				// session instead of erroring forever on a dead seam.
				// Unsettled deliveries redeliver — at-least-once semantics.
				r.releaseSessionSeam(ctx)
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

		if useSessions && len(raws) == 0 {
			// The held session is idle: rotate to the next available one
			// (the SDK's round-robin pattern) — but only once every
			// delivery received from it has settled; the outstanding
			// deliveries settle against this seam and closing it under
			// them would fail their Ack/Retry.
			if r.inFlightDeliveries.Load() == 0 {
				r.releaseSessionSeam(ctx)
			}
			continue
		}

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
			if useSessions {
				// Track settlement so idle rotation cannot close the seam
				// under an unsettled delivery. deliveryCtx is cancelled on
				// EVERY path — Ack/Retry (cleanupContext), emit failure
				// (cancel below), parent cancellation — so the counter
				// always drains.
				r.inFlightDeliveries.Add(1)
				context.AfterFunc(deliveryCtx, func() { r.inFlightDeliveries.Add(-1) })
			}
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
// commits it on success — but ONLY if no newer rotation superseded it
// in the meantime (commitRebuild fences on the captured generation). A
// superseded build is discarded (its stack closed) and reported as
// success: the newer rotation already installed a live stack.
func (r *Receiver) rebuildPendingStack(ctx context.Context) error {
	r.initMu.Lock()
	if !r.rebuildPending {
		r.initMu.Unlock()
		return nil
	}
	conn := r.pendingConn
	gen := r.rebuildGen
	r.initMu.Unlock()

	stack, err := r.build(ctx, conn)
	if err != nil {
		return fmt.Errorf("servicebus: retry pending session rebuild for %q: %w", r.entityName(), err)
	}

	toClose, applied := r.commitRebuild(gen, stack, conn)
	toClose.close(ctx)
	if !applied {
		// A newer rotation superseded this rebuild while it ran; the
		// freshly built stack was closed above. Nothing to commit.
		return nil
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug,
			"servicebus: pending session rebuild completed",
			"entity", r.entityName(),
			"session_id", r.cfg.SessionID,
		)
	}
	return nil
}

// ensureSessionSeam makes sure the poll loop holds a session receiver
// in use_sessions mode, accepting the NEXT available session when none
// is held. Holding no session is a normal state (idle entity, just
// rotated, a receive error shed the seam). The install is fenced on the
// client-handle identity: a credential rotation that swapped the stack
// while the accept dialled must not have its fresh handle paired with a
// session accepted on the OLD connection — the stale seam is discarded
// and the next iteration re-accepts on the live handle.
func (r *Receiver) ensureSessionSeam(ctx context.Context) error {
	r.initMu.Lock()
	if r.client != nil {
		r.initMu.Unlock()
		return nil
	}
	handle := r.asbClient
	accept := r.acceptNextFn
	r.initMu.Unlock()

	if accept == nil {
		if handle == nil {
			return shared.ErrUnavailable.WithMessage("servicebus: receiver not started")
		}
		sessOpts := asbSessionOptions{ReceiveAndDelete: r.cfg.receiveAndDelete()}
		if r.cfg.QueueName != "" {
			accept = func(ctx context.Context) (asbAPI, error) {
				return handle.AcceptNextSessionForQueue(ctx, r.cfg.QueueName, sessOpts)
			}
		} else {
			accept = func(ctx context.Context) (asbAPI, error) {
				return handle.AcceptNextSessionForSubscription(ctx, r.cfg.TopicName, r.cfg.SubscriptionName, sessOpts)
			}
		}
	}

	seam, err := accept(ctx)
	if err != nil {
		return err
	}

	r.initMu.Lock()
	if r.client == nil && r.asbClient == handle {
		r.client = seam
		r.initMu.Unlock()
		// A newly accepted session: bump the generation so the session
		// renewer resets its per-session failure budget and keeps
		// renewing this seam even if a PREVIOUS session's lock was
		// blipping when the renewer last ticked (F3).
		r.sessionGen.Add(1)
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "servicebus: accepted next session",
				"entity", r.entityName(),
			)
		}
		return nil
	}
	r.initMu.Unlock()

	// Superseded: a rotation swapped the stack (or Close emptied it)
	// while the accept dialled. Discard the stale session; nothing is
	// committed.
	if closer, ok := seam.(ports.ContextCloser); ok {
		_ = closer.Close(ctx)
	}
	return nil
}

// releaseSessionSeam drops and closes the currently held session
// receiver (use_sessions mode) so the next poll iteration accepts the
// NEXT available session — the rotation step of round-robin session
// consumption, also used to shed a seam whose link erred. It never
// touches an injected cfg.Client stack (no accept capability, so
// nothing to rotate to). The close runs outside the lock (closing an
// AMQP link can block).
func (r *Receiver) releaseSessionSeam(ctx context.Context) {
	r.initMu.Lock()
	if r.asbClient == nil && r.acceptNextFn == nil {
		r.initMu.Unlock()
		return
	}
	seam := r.client
	r.client = nil
	r.initMu.Unlock()

	if closer, ok := seam.(ports.ContextCloser); ok {
		_ = closer.Close(ctx)
	}
}

// runSessionRenewer renews the shared session lock at interval until
// ctx is done. One instance per poll loop in session mode; replaces
// the per-delivery auto-extend goroutines (which would all renew the
// SAME session lock redundantly).
//
// Unlike the per-delivery auto-extend loop, this renewer does NOT
// permanently exit on renewal failures — it exits ONLY when ctx is done
// (the poll loop stopping). Exiting early would deny renewal to every
// session the poll loop accepts afterwards, causing a LockLost
// redelivery storm (F3). Instead it:
//
//   - resets its consecutive-failure budget whenever a NEW session is
//     accepted (sessionGen changes), so a blip on a previous session
//     never starves the next one, and
//   - emits MetricASBLockRenewerStopped ONCE per degradation episode when
//     the current session's renewal fails autoExtendMaxFailures times in
//     a row, then keeps trying (paced at interval) so it recovers on the
//     next successful renewal or the next accepted session.
//
// The poll loop's receive-error path (releaseSessionSeam → re-accept) is
// the backstop that sheds a genuinely lost session; a shed leaves
// currentClient() nil, which the renewer skips without counting a
// failure, and the subsequent re-accept bumps sessionGen.
//
// It resolves the live receiver seam via currentClient() on EVERY tick
// rather than pinning a snapshot: a credential rotation swaps the stack
// under the still-running poll loop, and renewing the OLD (closed)
// client's session would leave the NEW session's lock unrenewed
// (lock-lost on in-flight deliveries).
func (r *Receiver) runSessionRenewer(ctx context.Context, interval time.Duration) {
	ticker := r.clock().NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	degradedSignalled := false
	lastGen := r.sessionGen.Load()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if gen := r.sessionGen.Load(); gen != lastGen {
				// A new session was accepted (rotation / re-accept): start
				// this session with a fresh failure budget so it is renewed
				// regardless of a previous session's blip.
				lastGen = gen
				consecutiveFailures = 0
				degradedSignalled = false
			}
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
				r.metrics.Counter(MetricASBLockRenewalFailures, 1)
				if r.logger != nil {
					r.logger.Warn("servicebus: session lock renewal failed",
						"session_id", r.cfg.SessionID,
						"error", err,
						"consecutive_failures", consecutiveFailures,
					)
				}
				if consecutiveFailures >= autoExtendMaxFailures && !degradedSignalled {
					// Persistent failure on the CURRENTLY held session:
					// alertable. Do NOT return — that would abandon every
					// session accepted afterwards. Signal once, keep
					// renewing; a successful renewal or a newly accepted
					// session clears the degraded state.
					degradedSignalled = true
					r.metrics.Counter(MetricASBLockRenewerStopped, 1,
						shared.Tag{Key: asbTagKeyRenewerScope, Value: asbRenewerScopeSession})
					if r.logger != nil {
						r.logger.Error("servicebus: session lock renewal persistently failing; lock may lapse and redeliver",
							"session_id", r.cfg.SessionID,
							"consecutive_failures", consecutiveFailures,
						)
					}
				}
				continue
			}
			consecutiveFailures = 0
			degradedSignalled = false
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
