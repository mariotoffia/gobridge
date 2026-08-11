package paho

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// handleConnectionUp records that the transport connection is
// (re)established and signals SessionConnected. autopaho invokes the
// OnConnectionUp callback (registered in Start) on every (re)connect,
// which calls this method.
//
// Ownership: the runtime session manager is the SINGLE
// owner of reconnect reconciliation. It reacts to the SessionConnected
// event by calling Reconcile, whose outcome is authoritative and whose
// failure propagates out of Manager.Run. This method must
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
// topic silently unsubscribed with no error surfaced.
//
// Lock discipline: this callback takes ONLY s.mu for the
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
// WITHOUT any new lock by the connEpoch generation counter: this reset
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
	if terminal != nil {
		// transitionTerminal already owns quiesce, disconnect and manager signal.
		// A deferred exclusive-error cleanup must not increment the epoch again.
		return terminal
	}
	if err := s.quiesceForRecycle(ctx); err != nil {
		return s.failClosedAfterQuiescence(ctx, err)
	}
	s.disconnectGeneration(ctx)
	return nil
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
