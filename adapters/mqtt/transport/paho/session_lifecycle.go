package paho

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
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

func (s *Session) awaitStartCleanup(ctx context.Context) {
	s.mu.Lock()
	starting := s.starting
	done := s.startDone
	hook := s.terminalAwaitStartHook
	s.mu.Unlock()
	if !starting || done == nil {
		return
	}
	if hook != nil {
		hook()
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
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
	drainGeneration := s.recoveryGeneration
	var drainDone <-chan struct{}
	drainOwner := false
	drainFinished := callerQuiesced
	if recoveryInFlight {
		if s.recoveryDrainGeneration != drainGeneration {
			s.recoveryDrainState = recoveryDrainNotStarted
			s.recoveryDrainGeneration = drainGeneration
			s.recoveryDrainDone = nil
		}
		switch s.recoveryDrainState {
		case recoveryDrainNotStarted:
			done := make(chan struct{})
			s.recoveryDrainState = recoveryDrainInProgress
			s.recoveryDrainDone = done
			drainDone = done
			drainOwner = true
			drainFinished = false
		case recoveryDrainInProgress:
			drainDone = s.recoveryDrainDone
			drainFinished = false
		case recoveryDrainFinished:
			drainDone = s.recoveryDrainDone
			drainFinished = true
		}
	}
	s.terminalErr = terminal
	if recoveryInFlight {
		s.recoveryErr = terminal
		s.recoveryPending = false
		s.recoveryAttemptActive = false
		s.recoveryNeedsSessionPresent = false
		s.recoverySessionPresentEpoch = 0
		s.recoveryTargetEpoch = 0
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
		switch {
		case recoveryInFlight && drainOwner:
			_ = s.quiesceForRecycle(terminalCtx)
			s.finishRecoveryDrain(drainGeneration, drainDone)
		case recoveryInFlight && !drainFinished && drainDone != nil:
			select {
			case <-drainDone:
			case <-terminalCtx.Done():
			}
		case !recoveryInFlight && !callerQuiesced:
			_ = s.quiesceForRecycle(terminalCtx)
		}
		s.awaitStartCleanup(terminalCtx)
		s.disconnectGeneration(terminalCtx)
		cancel()
		s.pushEvent(ports.SessionError, terminal)
		s.mu.Lock()
		s.clearRecoveryDrainLocked(drainGeneration)
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

// rejectIngressPoison synchronously latches terminal readiness and delegates
// bounded disconnect/cleanup to the existing Task 6 terminal lifecycle. It must
// return promptly because it runs on Paho's publish callback goroutine.
func (s *Session) rejectIngressPoison(cause error) {
	_, _ = s.transitionTerminal(context.Background(), cause, 0, true, false)
}

// rejectPredecodeIngress preserves the exact secret-safe guard cause in the
// terminal lifecycle before the guarded connection returns it to Paho. The
// connection closes synchronously after this callback, so OnConnectionDown
// observes terminalErr and existing reconnect policy stops the generation.
func (s *Session) rejectPredecodeIngress(cause error) {
	s.metrics.Counter(MetricMQTTRouterDropped, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if s.logger != nil {
		s.logger.Error("mqtt: rejected inbound packet before Paho decoding; terminating session",
			"client_id", s.opts.ClientID,
			"error", cause,
		)
	}
	s.rejectIngressPoison(cause)
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
	// await any in-flight Start BEFORE snapshotting s.cm. A
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
			// own). This does not regress, which only fires once our own
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
	s.router.noteConnectionTornDown()
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
		// the old CM was already torn down above, and this re-Start
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

// closeEventsLocked closes the session's lifecycle-event channel exactly
// once. TWO paths close it — Close (terminal shutdown) and Reload's
// Start-failure terminal signal — so a guard is required to avoid a
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
//  2. Stop the router. autopaho's Disconnect waits for the connection-manager
//     loop, which waits for the Paho client, which waits for every worker
//     goroutine — including the one running our publish callback. A callback
//     parked in the router (a saturated dispatch budget, or a slow delivery)
//     is released only by router.shutdown(), so disconnecting first would
//     park Disconnect behind it for the whole close deadline and return a
//     context error the session manager reads as a wedged close — which
//     retains the lease until its TTL instead of handing it to a standby.
//     Un-acked QoS 1/2 released here are redelivered on session resume.
//     This step is UNCONDITIONAL and precedes every bounded network wait, so
//     even a Close that returns its context error has definitively stopped this
//     session from dispatching or acknowledging ingress. The session manager
//     relies on that: it releases an exclusive lease once Close RETURNS, and a
//     returned-but-failed Close must not leave an owner still consuming.
//  3. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//  4. Await in-flight handlers (bounded by ctx), then close s.events —
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
	// Wake every detached session-lifetime wait (the settlement-recovery
	// cooldown runs on a context deliberately immune to route cancellation, so
	// this close is the only thing that can end it).
	if s.closedCh != nil {
		close(s.closedCh)
	}
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

	// Stop the router's grace-sweep and dispatch workers BEFORE disconnecting
	// (see the ordering invariant above) and before awaiting in-flight dispatch
	// handlers. shutdown signals the workers to exit, releases every parked
	// publish callback and marks the router closing; a best-effort orphan
	// UNSUBSCRIBE already in flight completes on its own (bounded by
	// orphanUnsubscribeTimeout) and is intentionally NOT awaited here, so Close
	// latency stays decoupled from a network round-trip.
	s.router.shutdown()

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

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
	handlersDrained := false
	select {
	case <-done:
		handlersDrained = true
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Warn("Close: context expired while waiting for in-flight handlers")
		}
	}
	// Close under s.mu via the shared guard: a prior Reload-failure may
	// already have closed s.events, so an unguarded close here would
	// double-close and panic. s.closed (set above) has already stopped every
	// pushEvent, so this only finalises the channel.
	s.mu.Lock()
	s.closeEventsLocked()
	s.mu.Unlock()

	if disconnErr != nil {
		return MapError(disconnErr)
	}
	if !handlersDrained {
		// Ingress IS stopped: router.shutdown() ran above, BEFORE the disconnect
		// and before any bounded wait, so no further message enters the route
		// pipeline however this returns. What is unfinished is the SETTLEMENT of
		// deliveries the pipeline already accepted, so report it rather than
		// success — an operator needs to see that the drain budget was too short.
		//
		// It is not a refusal to hand over the lease. The session manager gates a
		// hand-off on whether ingress is known to have stopped, which a returning
		// Close establishes either way; retaining a lease on every slow settle
		// would extend an outage to the full lease TTL on exactly the paths that
		// exist to recover from one. A straggling send from this owner is
		// version-fenced on outbox Complete and Claim: a duplicate at the
		// destination is possible, a double commit is not — the same
		// at-least-once window every failover already has.
		return shared.ErrTimeout.WithMessage(
			"mqtt: session close stopped ingress but gave up waiting for in-flight deliveries to settle")
	}
	return nil
}
