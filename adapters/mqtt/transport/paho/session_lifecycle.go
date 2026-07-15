package paho

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	settlementRecoveryDrainLimit  = 5 * time.Second
	settlementRecoveryMinInterval = 30 * time.Second
)

func (s *Session) acquireReload(ctx context.Context) error {
	select {
	case <-s.reloadGate:
		if s.reloadGateAcquiredHook != nil {
			s.reloadGateAcquiredHook()
		}
		return nil
	default:
	}
	if s.reloadGateWaitHook != nil {
		s.reloadGateWaitHook()
	}
	select {
	case <-s.reloadGate:
		if s.reloadGateAcquiredHook != nil {
			s.reloadGateAcquiredHook()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) releaseReload() { s.reloadGate <- struct{}{} }

func (s *Session) recoveryAttemptTimeout() time.Duration {
	s.mu.Lock()
	opts := s.opts
	mode := s.mode
	s.mu.Unlock()
	return (Config{Session: opts}).PostAcquireActivationTiming(mode).WorstCaseDuration
}

// contextWithClockTimeout applies a cancellable hard bound using the injected
// session clock, so recovery I/O observes deterministic cancellation in tests
// and no phase can outlive the adapter activation budget.
func (s *Session) contextWithClockTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if timeout <= 0 {
		cancel()
		return ctx, func() {}
	}
	timer := s.clock().NewTimer(timeout)
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			cancel()
		case <-done:
		}
	}()
	var cancelOnce sync.Once
	return ctx, func() {
		cancelOnce.Do(func() {
			timer.Stop()
			close(done)
			cancel()
		})
	}
}

func settlementRecoveryTerminalError(cause error) error {
	if cause == nil {
		cause = shared.ErrUnavailable.WithMessage("mqtt: settlement recovery failed")
	}
	return shared.ErrUnavailable.
		WithMessage("mqtt: settlement recovery failed; session is terminal").
		Wrap(errors.Join(cause, shared.ErrTransportClosedPermanently))
}

// transitionTerminal is the only terminal state writer. The first caller
// latches the cause and owns one bounded teardown; later causes coalesce. When a
// recovery is queued/active, every recovery field is cleared coherently even if
// a Task-5 failClosed path wins the race with the recovery finalizer.
func (s *Session) transitionTerminal(
	parent context.Context,
	cause error,
	recoveryGeneration uint64,
	async bool,
	callerQuiesced bool,
) (error, bool) {
	terminal := cause
	if terminal == nil {
		terminal = shared.ErrUnavailable.WithMessage("mqtt: session is terminal")
	}
	if !errors.Is(terminal, shared.ErrTransportClosedPermanently) {
		terminal = shared.ErrUnavailable.
			WithMessage("mqtt: session is terminal").
			Wrap(errors.Join(terminal, shared.ErrTransportClosedPermanently))
	}

	s.mu.Lock()
	if s.terminalErr != nil {
		latched := s.terminalErr
		s.mu.Unlock()
		return latched, false
	}
	recoveryInFlight := s.recoveryPending || s.recoveryAttemptActive
	if recoveryGeneration != 0 &&
		(s.recoveryGeneration != recoveryGeneration || !recoveryInFlight) {
		s.mu.Unlock()
		return terminal, false
	}
	skipQuiesce := callerQuiesced
	if recoveryInFlight {
		skipQuiesce = s.recoveryQuiesced &&
			s.recoveryQuiescedGeneration == s.recoveryGeneration
	}
	s.terminalErr = terminal
	if recoveryInFlight {
		s.recoveryErr = terminal
		s.recoveryPending = false
		s.recoveryAttemptActive = false
		s.recoveryNeedsSessionPresent = false
		s.recoverySessionPresentEpoch = 0
		s.recoveryTargetEpoch = 0
		s.recoveryQuiesced = false
		s.recoveryQuiescedGeneration = 0
		s.lastRecoveryCompleted = s.clock().Now()
	}
	cancelAttempt := s.recoveryAttemptCancel
	s.recoveryAttemptCancel = nil
	hook := s.recoveryQueuedFailureHook
	s.mu.Unlock()
	if cancelAttempt != nil {
		cancelAttempt()
	}
	if hook != nil && recoveryInFlight {
		hook()
	}

	finish := func() {
		terminalCtx, cancel := s.contextWithClockTimeout(context.WithoutCancel(parent), s.recoveryAttemptTimeout())
		if !skipQuiesce {
			_ = s.quiesceForRecycle(terminalCtx)
		}
		s.disconnectGeneration(terminalCtx)
		cancel()
		s.pushEvent(ports.SessionError, terminal)
		s.mu.Lock()
		s.closeEventsLocked()
		s.mu.Unlock()
	}
	if async {
		go finish()
	} else {
		finish()
	}
	return terminal, true
}

func (s *Session) terminateFailedRecovery(generation uint64, cause error, async bool) bool {
	terminal := settlementRecoveryTerminalError(cause)
	_, started := s.transitionTerminal(context.Background(), terminal, generation, async, false)
	return started
}

func (s *Session) failQueuedRecovery(generation uint64, attemptErr error) bool {
	return s.terminateFailedRecovery(generation, attemptErr, false)
}

func (s *Session) completeRecoveryAttempt(generation uint64, attemptErr error, success bool) bool {
	if !success {
		if attemptErr == nil {
			attemptErr = shared.ErrUnavailable.WithMessage("mqtt: settlement recovery did not complete")
		}
		return s.terminateFailedRecovery(generation, attemptErr, false)
	}

	s.mu.Lock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		s.mu.Unlock()
		return false
	}
	s.recoveryAttemptActive = false
	s.lastRecoveryCompleted = s.clock().Now()
	cancel := s.recoveryAttemptCancel
	s.recoveryAttemptCancel = nil
	s.recoveryPending = false
	s.recoveryNeedsSessionPresent = false
	s.recoverySessionPresentEpoch = 0
	s.recoveryTargetEpoch = 0
	s.recoveryQuiesced = false
	s.recoveryQuiescedGeneration = 0
	s.recoveryErr = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *Session) recordRecoveryRecycleStart(generation uint64) bool {
	s.mu.Lock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		s.mu.Unlock()
		return false
	}
	s.recoveryRecycleCount++
	s.mu.Unlock()
	s.metrics.Counter(MetricMQTTSessionRecoveryRecycle, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	return true
}

func (s *Session) captureRecoveryTargetEpoch(generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		return shared.ErrUnavailable.WithMessage("mqtt: settlement recovery attempt is no longer active")
	}
	targetEpoch := s.connEpoch
	s.recoveryTargetEpoch = targetEpoch
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	if targetEpoch == 0 || s.recoverySessionPresentEpoch != targetEpoch {
		return shared.ErrUnavailable.
			WithMessage("mqtt: Session Present evidence does not match recovery connection epoch").
			With("target_epoch", targetEpoch).
			With("session_present_epoch", s.recoverySessionPresentEpoch)
	}
	return nil
}

// requestRecovery records the Retry synchronously, then recycles the durable
// broker session asynchronously. Retry cannot perform the recycle inline: the
// recycle drain includes the RouteRunner settlement that is currently calling
// Retry, so waiting here would deadlock that drain.
func (s *Session) requestRecovery(ctx context.Context) error {
	if s.mode == connectivity.SessionEphemeral {
		return shared.ErrNotSupported
	}
	s.mu.Lock()
	if s.terminalErr != nil {
		terminal := s.terminalErr
		s.mu.Unlock()
		return terminal
	}
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt: session recovery requested after session closure")
	}
	if s.recoveryPending {
		s.mu.Unlock()
		return nil
	}
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.recoverySessionPresentEpoch = 0
	s.recoveryQuiesced = false
	s.recoveryQuiescedGeneration = 0
	s.recoveryGeneration++
	generation := s.recoveryGeneration
	s.recoveryErr = nil
	s.subscriptionsSatisfied = false
	var rateLimit <-chan time.Time
	if !s.lastRecoveryCompleted.IsZero() {
		elapsed := s.clock().Since(s.lastRecoveryCompleted)
		if elapsed < settlementRecoveryMinInterval {
			rateLimit = s.clock().After(settlementRecoveryMinInterval - elapsed)
		}
	}
	s.mu.Unlock()

	go s.runRecovery(context.WithoutCancel(ctx), rateLimit, generation)
	return nil
}

func (s *Session) runRecovery(ctx context.Context, rateLimit <-chan time.Time, generation uint64) {
	if rateLimit != nil {
		select {
		case <-rateLimit:
		case <-ctx.Done():
			return
		}
	}
	attemptTimeout := s.recoveryAttemptTimeout()
	attemptCtx, cancelAttempt := s.contextWithClockTimeout(ctx, attemptTimeout)
	ctx = attemptCtx
	if err := s.acquireReload(ctx); err != nil {
		mapped := MapError(err).WithMessage("mqtt: settlement recovery waiting for session serialization")
		cancelAttempt()
		s.failQueuedRecovery(generation, mapped)
		return
	}
	defer s.releaseReload()
	if err := ctx.Err(); err != nil {
		cancelAttempt()
		s.failQueuedRecovery(generation,
			MapError(err).WithMessage("mqtt: settlement recovery cancelled after serialization"))
		return
	}

	// Request state becomes attempt state only after owning the single session
	// gate. An ordinary Reconcile that ran before this point saw only degraded,
	// coalescing request state and could not validate or complete this recovery.
	s.mu.Lock()
	if !s.recoveryPending || s.recoveryGeneration != generation {
		s.mu.Unlock()
		cancelAttempt()
		return
	}
	if s.closed || s.terminalErr != nil || s.recoveryErr != nil {
		queuedErr := s.recoveryErr
		if queuedErr == nil {
			queuedErr = s.terminalErr
		}
		if queuedErr == nil {
			queuedErr = shared.ErrUnavailable.WithMessage("mqtt: session closed before queued recovery began")
		}
		s.mu.Unlock()
		cancelAttempt()
		s.failQueuedRecovery(generation, queuedErr)
		return
	}
	s.recoveryAttemptActive = true
	s.recoveryQuiesced = false
	s.recoveryQuiescedGeneration = 0
	s.recoveryAttemptCancel = cancelAttempt
	s.recoveryNeedsSessionPresent = true
	s.recoverySessionPresentEpoch = 0
	s.recoveryTargetEpoch = 0
	s.mu.Unlock()

	// Stop new ingress immediately, but let already accepted route work settle
	// for a bounded interval before the old socket is detached.
	drainCtx, cancelDrain := context.WithCancel(ctx)
	drainDone := make(chan struct{})
	drainLimit := s.clock().NewTimer(settlementRecoveryDrainLimit)
	go func() {
		select {
		case <-drainLimit.C():
			cancelDrain()
		case <-drainDone:
		}
	}()
	drainErr := s.quiesceForRecycle(drainCtx)
	close(drainDone)
	drainLimit.Stop()
	cancelDrain()
	if drainErr != nil {
		s.completeRecoveryAttempt(generation,
			shared.ErrUnavailable.WithMessage("mqtt: settlement recovery drain failed").Wrap(drainErr), false)
		return
	}
	s.mu.Lock()
	if s.recoveryAttemptActive && s.recoveryGeneration == generation {
		s.recoveryQuiesced = true
		s.recoveryQuiescedGeneration = generation
	}
	s.mu.Unlock()

	if !s.recordRecoveryRecycleStart(generation) {
		return
	}
	if err := s.reloadLocked(ctx); err != nil {
		s.completeRecoveryAttempt(generation, err, false)
		return
	}
	if err := s.captureRecoveryTargetEpoch(generation); err != nil {
		s.completeRecoveryAttempt(generation, err, false)
		return
	}

	// Recovery owns the same gate as ordinary Reconcile, so converge the saved
	// plan through the private under-gate helper without publishing an event that
	// would need to reacquire serialization. The runtime manager may perform a
	// later idempotent Reconcile after this gate is released.
	s.mu.Lock()
	plan := connectivity.SessionPlan{}
	if s.plan != nil {
		plan = cloneSessionPlan(*s.plan)
	}
	s.mu.Unlock()
	_ = s.reconcileUnderGate(ctx, plan, generation)
}

// Reload tears down the current ConnectionManager and re-runs Start
// with the session's current options. It is intended for rotation
// scenarios that cannot be applied to an existing CM — most notably
// TLS material, which requires a new tls.Config baked into a fresh
// autopaho configuration.
//
// Semantics:
//   - Session state is preserved: the plan, router, and event channel
//     survive the teardown. Subscribers stay subscribed (activeSubs
//     is re-materialised by reconcile when the new CM connects).
//   - liveCreds is preserved, so username/password rotation that has
//     already been applied via ApplyCredentials is not lost.
//   - Close-after-Reload semantics match Close: once closed, Reload
//     returns ErrUnavailable.
//
// Why not named Restart: "Restart" would imply a user-initiated
// restart of the session's configured lifecycle, including re-parsing
// options. Reload is narrower — the options are not re-read, only the
// TLS handshake + transport layer are rebuilt.
func (s *Session) Reload(ctx context.Context) error {
	if err := s.acquireReload(ctx); err != nil {
		return MapError(err).WithMessage("mqtt reload: waiting for serialization gate")
	}
	defer s.releaseReload()
	return s.reloadLocked(ctx)
}

// reloadLocked performs one ConnectionManager replacement while the caller owns
// reloadGate, the shared ConnectionManager-reload serialization gate.
func (s *Session) reloadLocked(ctx context.Context) error {
	s.mu.Lock()
	if s.terminalErr != nil {
		terminal := s.terminalErr
		s.mu.Unlock()
		return terminal
	}
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt session is closed; Reload is not allowed after Close")
	}
	// FIX 1: await any in-flight Start BEFORE snapshotting s.cm. A
	// supervisor-driven Start (superviseSession re-Run) can be mid-dial with
	// s.cm not yet installed; if Reload snapshotted s.cm now it would see nil,
	// SKIP the teardown, then call its own Start — which the Start guard
	// (acl_session.go) makes PIGGYBACK on the in-flight dial (returns nil for
	// the second caller). The rotation's teardown+rebuild would be DEFEATED and
	// the previously-dialed connection — carrying the OLD per-dial TLS snapshot
	// (dial captures tls.Config per attempt) — would stay live. Mirror Close's
	// in-flight-Start wait: snapshot starting+startDone, release mu, wait on
	// startDone bounded by ctx, then re-acquire mu and tear the now-installed cm
	// down. Handles both outcomes: the in-flight Start SUCCEEDED (we now see and
	// tear down its freshly installed cm, then rebuild) or FAILED (cm is nil →
	// we proceed to a fresh Start below, exactly as before).
	starting := s.starting
	startDone := s.startDone
	s.mu.Unlock()

	if starting && startDone != nil {
		select {
		case <-startDone:
		case <-ctx.Done():
			// Abort BEFORE any teardown — nothing was disconnected, so the
			// session is left intact (the in-flight Start completes on its
			// own). This does not regress F-1, which only fires once our own
			// post-teardown rebuild Start fails.
			return MapError(ctx.Err()).
				WithMessage("mqtt reload: context expired awaiting in-flight Start")
		}
	}

	s.mu.Lock()
	// A Close may have landed while we awaited the in-flight Start.
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt session is closed; Reload is not allowed after Close")
	}
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.connected = false
	// Invalidate readiness and the old connection generation before Start can
	// return with a replacement CM whose OnConnectionUp callback is still queued.
	// A prior-generation reconcile then cannot write into replacement state.
	s.subscriptionsSatisfied = false
	s.observedSubs = make(map[string]subscriptionGrant)
	s.activeSubs = make(map[string]byte)
	s.connEpoch++
	s.mu.Unlock()

	var disconnectErr error
	if cm != nil {
		disconnectErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}
	if disconnectErr != nil {
		mapped := MapError(disconnectErr).WithMessage("mqtt reload: disconnect current generation")
		s.mu.Lock()
		recoveryOwnsTerminalSignal := s.recoveryPending ||
			(s.recoveryGeneration > 0 && s.terminalErr != nil && s.recoveryErr != nil)
		if !recoveryOwnsTerminalSignal {
			s.closeEventsLocked()
		}
		s.mu.Unlock()
		return mapped
	}

	if err := s.Start(ctx); err != nil {
		// F-1: the old CM was already torn down above, and this re-Start
		// failed (e.g. the broker is down during a credential-rotation
		// Reload). The session is now DEAD — with no CM there is no
		// autopaho reconnect and no further SessionEvent, so the runtime
		// manager's handleEvents would block on <-events forever and the
		// supervisor would never restart it (readiness red, liveness
		// green — a permanent zombie). Signal terminal death by CLOSING
		// the events channel: the runtime session manager treats an
		// unexpected events-channel close as a session FAILURE
		// (errSessionEventsClosed) and superviseSession re-invokes
		// Manager.Run, which re-Starts this session and rebuilds the CM.
		// autopaho then retries the connection to recovery once the broker
		// returns. The re-Start re-materialises a fresh events channel
		// (see Start's eventsClosed handling), so handleEvents does not
		// spin on a closed channel. The close is guarded (closeEventsLocked)
		// so a concurrent/subsequent Close cannot double-close it.
		s.mu.Lock()
		recoveryOwnsTerminalSignal := s.recoveryPending ||
			(s.recoveryGeneration > 0 && s.terminalErr != nil && s.recoveryErr != nil)
		if !recoveryOwnsTerminalSignal {
			s.closeEventsLocked()
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// quiesceForRecycle atomically stops new router callback acceptance, waits for
// accepted callbacks to return, then waits for the runtime RouteRunner settlement
// counters. The reconcile/session context is always honored; ReconcileTimeout is
// an additional ceiling for direct callers that supplied no shorter deadline.
func (s *Session) quiesceForRecycle(ctx context.Context) error {
	s.mu.Lock()
	waiter := s.ingressQuiescenceWaiter
	s.mu.Unlock()
	if s.router == nil {
		if waiter != nil {
			return waiter(ctx)
		}
		return nil
	}
	quiesceCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
	defer cancel()
	return s.router.quiesceForRecycle(quiesceCtx, waiter)
}

func terminalIngressQuiescenceError(cause error) error {
	return shared.ErrUnavailable.
		WithMessage("mqtt: source ingress did not quiesce before connection recycle; session is fail-closed").
		Wrap(errors.Join(cause, shared.ErrTransportClosedPermanently))
}

// failClosedAfterQuiescence disconnects the broker generation immediately and
// permanently latches this Session instance. Runtime/session translates the
// permanent marker to ErrSessionUnrecoverable; exclusive owners then retain the
// lease until natural TTL instead of releasing while old route work can mutate.
func (s *Session) failClosedAfterQuiescence(ctx context.Context, cause error) error {
	return s.failClosed(ctx, terminalIngressQuiescenceError(cause))
}

func managedMigrationRequiredError() error {
	return shared.ErrUnavailable.
		WithMessage("mqtt: managed subscription migration cannot safely settle a broker-pinned delivery; restore the old configuration and exact handler, drain it, then retry the cutover").
		Wrap(shared.ErrTransportClosedPermanently)
}

func (s *Session) failClosedForManagedMigration(ctx context.Context) error {
	return s.failClosed(ctx, managedMigrationRequiredError())
}

func (s *Session) failClosed(ctx context.Context, cause error) error {
	terminal, _ := s.transitionTerminal(ctx, cause, 0, false, true)
	return terminal
}

// disconnectFailedReconcile detaches the current generation and tears down its
// connection without terminally closing the Session when ingress drains safely.
// A failed drain is unrecoverable in-process and is returned to the manager.
func (s *Session) disconnectFailedReconcile(ctx context.Context) error {
	s.mu.Lock()
	terminal := s.terminalErr
	s.mu.Unlock()
	if terminal == nil {
		if err := s.quiesceForRecycle(ctx); err != nil {
			return s.failClosedAfterQuiescence(ctx, err)
		}
	}
	s.disconnectGeneration(ctx)
	return terminal
}

func (s *Session) disconnectGeneration(ctx context.Context) {
	s.mu.Lock()
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.connected = false
	s.subscriptionsSatisfied = false
	s.observedSubs = make(map[string]subscriptionGrant)
	s.activeSubs = make(map[string]byte)
	s.connEpoch++
	s.mu.Unlock()

	disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.reconcileTimeout())
	defer cancel()
	if cm != nil {
		_ = cm.Disconnect(disconnectCtx)
	}
	if cmCancel != nil {
		cmCancel()
	}
}

// closeEventsLocked closes the session's lifecycle-event channel exactly
// once. TWO paths close it — Close (terminal shutdown) and Reload's
// Start-failure terminal signal (F-1) — so a guard is required to avoid a
// double-close panic when both run (e.g. a Close landing after a
// Reload-failure already closed events). pushEvent also checks
// s.eventsClosed under s.mu, so no concurrent send can race this close.
// Callers MUST hold s.mu.
func (s *Session) closeEventsLocked() {
	if s.eventsClosed {
		return
	}
	s.eventsClosed = true
	close(s.events)
}

// Close gracefully disconnects the MQTT session. It is safe to call
// Close multiple times.
//
// Ordering invariant (do not reorder):
//  1. Set s.closed = true under mutex — prevents pushEvent from sending.
//  2. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//  3. Await in-flight handlers (bounded by ctx), then close s.events —
//     safe because step 1 guarantees no concurrent sender can reach the
//     channel send.
//
// Delivery semantics on Close: the adapter uses manual acknowledgment
// (see delivery.go), so a publish whose Delivery has not been settled
// when the socket tears down is simply never acked — the broker
// redelivers it to the next owner of a persistent/exclusive session.
// Close does NOT drain: it disconnects, waits (bounded by ctx) for
// handler goroutines to return, and leaves unsettled deliveries to
// broker redelivery — at-least-once either way, with downstream
// idempotency/dedup absorbing the duplicates.
func (s *Session) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session closing",
			"client_id", s.opts.ClientID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	if s.starting && !s.connectionUpCompleted {
		s.connectionUpErr = shared.ErrUnavailable.WithMessage("mqtt: session closed before connection-up callback completed")
		s.connectionUpCompleted = true
		if s.connectionUpDone != nil {
			close(s.connectionUpDone)
		}
		// Any callback queued by the discarded construction is stale.
		s.connectionGeneration++
	}
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	// Snapshot any in-flight Start so we can wait for it to observe
	// s.closed and tear down its own freshly built CM before we finish
	// closing — otherwise Close could return while a half-built CM is
	// still connecting, which would then install/leak as a zombie CM.
	starting := s.starting
	startDone := s.startDone
	s.mu.Unlock()

	// Wait (bounded by ctx) for the in-flight Start to finish. With
	// s.closed already set, that Start's post-AwaitConnection re-check
	// discards its CM instead of installing it, so once startDone closes
	// no zombie ConnectionManager can survive this Close.
	if starting && startDone != nil {
		select {
		case <-startDone:
		case <-ctx.Done():
			if s.logger != nil {
				s.logger.Warn("Close: context expired while waiting for in-flight Start")
			}
		}
	}

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	// Stop the router's grace-sweep and dispatch workers before awaiting
	// in-flight dispatch handlers. shutdown signals the workers to exit and
	// marks the router closing; a best-effort orphan UNSUBSCRIBE already in
	// flight completes on its own (bounded by orphanUnsubscribeTimeout) and
	// is intentionally NOT awaited here, so Close latency stays decoupled
	// from a network round-trip.
	s.router.shutdown()

	done := make(chan struct{})
	go func() {
		// Join the serialized dispatch worker FIRST: its final fanout
		// r.wg.Add must complete before r.wg.Wait, otherwise a buffered
		// item drained after shutdown would Add concurrently with Wait
		// (WaitGroup Add-during-Wait → panic). Then await the handler
		// WaitGroup. Both are bounded by the outer ctx via this select.
		s.router.awaitDispatchLoop()
		s.router.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Warn("Close: context expired while waiting for in-flight handlers")
		}
	}
	// Close under s.mu via the shared guard: a prior Reload-failure may
	// already have closed s.events (F-1), so an unguarded close here would
	// double-close and panic. s.closed (set above) has already stopped every
	// pushEvent, so this only finalises the channel.
	s.mu.Lock()
	s.closeEventsLocked()
	s.mu.Unlock()

	if disconnErr != nil {
		return MapError(disconnErr)
	}
	return nil
}

// handleConnectionUp records that the transport connection is
// (re)established and signals SessionConnected. autopaho invokes the
// OnConnectionUp callback (registered in Start) on every (re)connect,
// which calls this method.
//
// Ownership (finding C7): the runtime session manager is the SINGLE
// owner of reconnect reconciliation. It reacts to the SessionConnected
// event by calling Reconcile, whose outcome is authoritative and whose
// failure propagates out of Manager.Run (finding S9). This method must
// therefore NOT reconcile inline; it only resets local subscription
// state and emits the event.
//
// Ordering is load-bearing: activeSubs is reset to empty BEFORE
// SessionConnected is emitted. The reset happens-before the event, which
// happens-before the manager's reconcile reads activeSubs, so on
// reconnect the manager always observes an empty set and issues a full,
// authoritative re-subscribe. The previous ordering emitted the event
// first, letting the manager's reconcile observe stale subscriptions,
// compute an empty delta, and skip the re-subscribe — which, combined
// with an inline reconcile that swallowed its own failure, could leave a
// topic silently unsubscribed with no error surfaced (finding C7).
//
// Lock discipline (finding C7-N4): this callback takes ONLY s.mu for the
// subscription-state reset and MUST NOT acquire reloadGate. autopaho invokes
// OnConnectionUp synchronously on its sole connection-management goroutine and
// documents that the callback "must not block" (autopaho.ClientConfig.
// OnConnectionUp, auto.go). reloadGate is held by Reconcile for the entire
// duration of a network SUBSCRIBE / UNSUBSCRIBE round-trip, so blocking on it
// here would stall that goroutine — the owner of reconnect and error handling
// — on a network call, violating the SDK contract and coupling liveness to
// paho's in-flight-request teardown behaviour.
//
// The TOCTOU a lock here would close — a prior-connection reconcile writing
// stale subscription state AFTER this reset — is instead closed
// WITHOUT any new lock by the connEpoch generation counter (A-3): this reset
// bumps s.connEpoch, and reconcile skips any write-back whose captured epoch no
// longer matches. That keeps activeSubs empty for the new connection, so the
// authoritative reconnect reconcile issues a full re-subscribe rather than
// computing an empty delta against stale state and silently dropping
// subscriptions on an ephemeral (clean_start) session. Do NOT add reloadGate
// here — the epoch guard is the deadlock-free closure.
func (s *Session) handleConnectionUp() {
	s.mu.Lock()
	generation := s.connectionGeneration
	s.mu.Unlock()
	s.handleConnectionUpGeneration(generation)
}

func (s *Session) handleConnectionUpGeneration(generation uint64) {
	s.handleConnectionUpGenerationWithSessionPresent(generation, true)
}

func (s *Session) handleConnectionUpGenerationWithSessionPresent(generation uint64, sessionPresent bool) {
	s.mu.Lock()
	if generation != s.connectionGeneration || s.closed {
		s.mu.Unlock()
		return
	}
	if s.terminalErr != nil {
		err := s.terminalErr
		s.mu.Unlock()
		s.completeConnectionUpBarrier(generation, err)
		return
	}
	if s.recoveryNeedsSessionPresent && !sessionPresent {
		err := shared.ErrUnavailable.WithMessage(
			"mqtt: settlement recovery did not resume the broker session (Session Present=false)")
		recoveryGeneration := s.recoveryGeneration
		s.connected = false
		s.subscriptionsSatisfied = false
		s.mu.Unlock()
		s.terminateFailedRecovery(recoveryGeneration, err, true)
		s.mu.Lock()
		terminal := s.terminalErr
		s.mu.Unlock()
		s.completeConnectionUpBarrier(generation, terminal)
		return
	}
	nextEpoch := s.connEpoch + 1
	if s.recoveryNeedsSessionPresent {
		s.recoverySessionPresentEpoch = nextEpoch
		s.recoveryErr = nil
	}
	s.connected = true
	s.connUpAt = s.clock().Now().UnixNano()
	s.observedSubs = make(map[string]subscriptionGrant)
	s.activeSubs = make(map[string]byte)
	s.subscriptionsSatisfied = false
	s.connEpoch = nextEpoch
	s.mu.Unlock()

	// This reset is part of connection-up completion: Start/Reload must not
	// return until the replacement router epoch is active.
	s.router.beginGrace()

	s.completeConnectionUpBarrier(generation, nil)
	s.mu.Lock()
	current := generation == s.connectionGeneration && s.connectionUpErr == nil && s.connected && !s.closed
	s.mu.Unlock()
	if !current {
		return
	}
	s.pushEvent(ports.SessionConnected, nil)

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection up",
			"client_id", s.opts.ClientID)
	}
}

func (s *Session) completeConnectionUpBarrier(generation uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.connectionGeneration || s.connectionUpCompleted {
		return
	}
	s.connectionUpErr = err
	s.connectionUpCompleted = true
	if s.connectionUpDone != nil {
		close(s.connectionUpDone)
	}
}

func (s *Session) invalidateConnectionGeneration(generation uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.connectionGeneration {
		return
	}
	if !s.connectionUpCompleted {
		s.connectionUpErr = err
		s.connectionUpCompleted = true
		if s.connectionUpDone != nil {
			close(s.connectionUpDone)
		}
	}
	// Ignore callbacks still queued by the discarded ConnectionManager.
	s.connectionGeneration++
}

func (s *Session) handleConnectionDownGeneration(generation uint64) bool {
	s.mu.Lock()
	if generation != s.connectionGeneration || s.closed {
		s.mu.Unlock()
		return false
	}
	s.connected = false
	s.subscriptionsSatisfied = false
	if !s.connectionUpCompleted {
		s.connectionUpErr = shared.ErrUnavailable.WithMessage("mqtt: connection closed before connection-up callback completed")
		s.connectionUpCompleted = true
		if s.connectionUpDone != nil {
			close(s.connectionUpDone)
		}
	}
	s.mu.Unlock()
	s.pushEvent(ports.SessionDisconnected, nil)
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection down",
			"client_id", s.opts.ClientID)
	}
	return true
}

// disconnectSessionTakenOver is the MQTT v5 DISCONNECT reason code the
// broker sends to the OLD connection when another client connects with
// the same ClientID (spec §3.14.2.1; packets.DisconnectSessionTakenOver).
const disconnectSessionTakenOver byte = 0x8E

// takeoverStabilityWindow is how long a connection must have been up
// for a subsequent takeover to be considered a NEW incident (legit
// failover) rather than the continuation of a ClientID-collision storm.
const takeoverStabilityWindow = 30 * time.Second

// handleServerDisconnect observes server-initiated DISCONNECT packets
// (autopaho invokes the OnServerDisconnect hook in its own goroutine).
// Session takeover (0x8E) feeds the collision damping in
// noteSessionTakeover; every other reason code is logged — connection-
// down bookkeeping and events are owned by OnConnectionDown /
// OnConnectError, so no state is mutated here to avoid double signals.
func (s *Session) handleServerDisconnect(code byte) {
	if code == disconnectSessionTakenOver {
		s.noteSessionTakeover()
		return
	}
	if berr := MapDisconnectReasonCode(code); berr != nil && s.logger != nil {
		s.logger.Warn("mqtt: server disconnected the session",
			"client_id", s.opts.ClientID,
			"reason_code", code,
			"error", berr,
		)
	}
}

// noteSessionTakeover counts consecutive session-takeover disconnects
// and dampens the resulting reconnect storm. One takeover is normal
// (Exclusive failover: the standby legitimately claims the ClientID),
// so the first occurrence carries no penalty. Repeated takeovers
// without an intervening stable connection (>= takeoverStabilityWindow
// of uptime) mean two live instances share a client_id and are mutually
// kicking each other; each additional occurrence doubles the reconnect
// backoff penalty (1s, 2s, ... capped at 64s — see takeoverPenalty) and
// from the third occurrence an explicit Error log names the
// misconfiguration. MetricMQTTSessionTakeover counts every occurrence.
//
// Exception (HIGH-3): when shared subscriptions ($share) are active and the
// mode is NOT Exclusive, a single takeover already proves the scale-out
// client_id-collision self-DOS (replicas that must be unique are sharing an
// identity), so the Error log fires on the FIRST occurrence and names the
// unique-client_id requirement. The backoff penalty is unchanged (still
// streak-driven) — only the log severity/message is escalated.
func (s *Session) noteSessionTakeover() {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	if s.connUpAt != 0 && now-s.connUpAt >= int64(takeoverStabilityWindow) {
		// The connection had been stable: treat this as a fresh
		// incident, not a continuation of a collision storm.
		s.takeoverStreak = 0
	}
	s.takeoverStreak++
	s.lastTakeoverAt = now
	streak := s.takeoverStreak
	// A takeover while shared subscriptions ($share) are active is the
	// smoking gun of the HIGH-3 self-DOS: another instance connected with the
	// SAME client_id where scale-out demands UNIQUE ones. The only mode where
	// a shared client_id is legitimate is Exclusive (a single leaseholder, so
	// a takeover there is a normal lease handoff, not a collision). For every
	// other mode, escalate on the FIRST occurrence rather than waiting for the
	// streak to prove a storm.
	sharedSubsActive := s.planHasSharedSubscriptionsLocked()
	s.mu.Unlock()

	sharedSubCollision := sharedSubsActive && s.mode != connectivity.SessionExclusive

	s.metrics.Counter(MetricMQTTSessionTakeover, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})

	berr := MapDisconnectReasonCode(disconnectSessionTakenOver).
		With("takeover_count", streak)
	s.pushEvent(ports.SessionReconnecting, berr)

	if s.logger != nil {
		switch {
		case sharedSubCollision:
			s.logger.Error("mqtt: session takeover while shared subscriptions ($share) are active — another "+
				"instance is using the SAME client_id where scale-out REQUIRES a UNIQUE client_id per instance; "+
				"the instances are taking each other over (self-DOS) and load-balancing is broken. Give each "+
				"replica a unique client_id, or move to an exclusive lease if a single active owner is intended",
				"client_id", s.opts.ClientID,
				"session_mode", s.mode,
				"takeover_count", streak,
				"backoff_penalty", s.takeoverPenalty(),
			)
		case streak >= 3:
			s.logger.Error("mqtt: repeated session takeover — another live instance is using the same client_id; "+
				"reconnects are being backed off (fix the client_id collision or the instances will keep kicking each other)",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
				"backoff_penalty", s.takeoverPenalty(),
			)
		default:
			s.logger.Warn("mqtt: session taken over by another connection with the same client_id",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
			)
		}
	}
}

// takeoverPenalty returns the extra reconnect delay applied on top of
// the configured backoff while a takeover storm is IN PROGRESS:
// 0 for streak <= 1 (single takeover = legitimate failover; the standby
// must not be slowed down), then 1s << (streak-2) capped at 64s.
//
// The penalty is gated on recency (A-4): it only applies while takeovers are
// still actively arriving (the last one within takeoverStabilityWindow). Once
// the collision resolves and no takeover has occurred for that window, the
// penalty decays to 0 even though takeoverStreak is still high — an ordinary
// reconnect (a network blip long after the storm) must not be stuck paying a
// stale storm's backoff. The streak itself is only reset when a NEW takeover
// arrives after a stable connection (noteSessionTakeover), so a takeover
// recurring within the window resumes damping at the accumulated level.
func (s *Session) takeoverPenalty() time.Duration {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	streak := s.takeoverStreak
	last := s.lastTakeoverAt
	s.mu.Unlock()
	if streak <= 1 || last == 0 || now-last >= int64(takeoverStabilityWindow) {
		return 0
	}
	shift := streak - 2
	if shift > 6 {
		shift = 6
	}
	return time.Duration(1<<shift) * time.Second
}
