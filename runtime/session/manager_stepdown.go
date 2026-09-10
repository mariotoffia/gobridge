package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/logging"
)

// Surrendering a term: reconnecting a source between lease gaps, the three-phase
// step-down, and the classification of every non-context renewal-loop exit into
// a lease transfer, a session failure, or a terminal wedge.

// ensureConnected re-establishes the source session when it is not currently
// connected — typically because a previous term's stepDown closed it to stop
// a non-owner from consuming source messages. It is Health-gated, so it is a
// no-op for an already-connected session and is therefore safe to call on
// every lease re-acquisition. The defensive Close clears any half-open state
// before restarting and is idempotent.
func (m *Manager) ensureConnected(ctx context.Context) error {
	if m.session.Health(ctx).Connected {
		return nil
	}
	m.log(ctx, slog.LevelWarn, "session disconnected during lease gap, reconnecting")
	if closeErr := m.session.Close(ctx); closeErr != nil {
		m.log(ctx, slog.LevelWarn, "session close failed before restart", "error", closeErr)
	}
	if err := m.session.Start(ctx); err != nil {
		return err
	}
	return nil
}

// stepDown implements the three-phase step-down protocol:
// 1. Stop claiming new outbox entries (clear hasLease)
// 2. Wait grace period for in-flight completions
// 3. Release the lease
func (m *Manager) stepDown(ctx context.Context) error {
	token, closed := m.beginStepDown(ctx)
	return m.finishStepDown(ctx, token, closed)
}

// beginStepDown removes local authorization and disconnects the source before
// any release can occur. It is split from finishStepDown so lease loss during
// activation can wait for the activation callback to settle before hand-off.
func (m *Manager) beginStepDown(ctx context.Context) (persistence.LeaseToken, bool) {
	m.log(ctx, slog.LevelWarn, "stepping down from lease")

	if logging.DebugEnabled(m.logger) {
		m.logger.Log(ctx, logging.LevelDebug, "step-down initiated",
			"session_id", m.sessionID,
			"reason", "renewal failures exceeded max",
		)
	}

	m.mu.Lock()
	token := m.token
	m.hasLease = false
	m.mu.Unlock()

	m.emitLeaseAudit(ctx, "lease.stepdown", "success", token, nil)
	m.pushLeaseEvent(LeaseStateSteppedDown, token, nil)
	_, closed := m.closeSourceBounded(ctx, m.releaseTimeout(), "lease step-down")
	return token, closed
}

// finishStepDown waits the existing settlement grace and releases only after a
// completed source disconnect. A wedged Close is terminal and retains the lease
// until natural expiry.
func (m *Manager) finishStepDown(ctx context.Context, token persistence.LeaseToken, closeCompleted bool) error {
	if !closeCompleted {
		return fmt.Errorf("%w: %w", ErrSessionUnrecoverable, errStepDownCloseFailed)
	}
	m.awaitSettlementGrace(ctx)
	m.releaseOwnedLeaseBestEffort(ctx, token, "step-down")
	return errLeaseLostAfterRenewal
}

// awaitSettlementGrace holds the lease for the bounded StepDownGrace before it
// is released, so destination work this owner already ACCEPTED can settle
// before the standby that acquires next advances the fence. Closing the source
// stops INGRESS; it does not settle the sends already taken from it, and a send
// completing after the fence moves is an accepted duplicate.
//
// Every path that surrenders a held lease while such work may be outstanding
// uses it: the three-phase step-down and the session-failure recovery. It is
// skipped on one piece of evidence — a destination drainer that reports idle,
// which proves there is nothing left to settle, so waiting would only add
// takeover latency (a new owner keys off the lease store, not this wait).
//
// The wait is detached from ctx so a shutdown cannot cut it short, and bounded
// by StepDownGrace so a wedged destination cannot hold the partition forever.
func (m *Manager) awaitSettlementGrace(ctx context.Context) {
	if m.drainIdle != nil && m.drainIdle() {
		return
	}
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), m.stepDownGrace)
	defer graceCancel()
	<-graceCtx.Done()
}

// afterRenewLoopExit classifies renewLoop's non-ctx return and emits the
// matching observability signal. It returns the error the caller must PROPAGATE
// (return from Run), or nil when the caller should re-acquire the lease in place.
//
//   - Genuine lease loss (stepDown crossed the renewal-failure threshold or a
//     definitive fencing signal, errLeaseLostAfterRenewal): emit lease.lost +
//     LeaseStateLost, clear hasLease, and return nil so the caller re-acquires.
//     This — and only this — is a real lease transfer (MetricLeaseTransfers on
//     re-acquire). stepDown has already released the lease, so the re-acquire is
//     unimpeded.
//
//   - Session failure (any other non-nil err: a reconcile-on-reconnect failure
//     or an unexpected Events-channel close): the lease is still held and
//     unexpired, so this is NOT a lease loss. Emit the DISTINCT
//     LeaseStateReconcileFailed signal (never lease.lost / LeaseStateLost / a
//     MetricLeaseTransfers-bearing re-acquire — MetricReconcileFailures was
//     already emitted at the failure site) and return the error so the caller
//     surfaces it to superviseSession for isolated restart, keeping
//     lease-transfer observability uncontaminated. hasLease is cleared
//     so a drainer does not act on a lease this failing session is no longer
//     renewing during the restart window.
//
// Release-then-reacquire recovery: on a session failure the
// still-held lease is RELEASED best-effort here before the restart, so the
// restarted Run re-acquires immediately instead of blocking in Acquire against
// our own unexpired lease until LeaseTTL self-expiry. Fencing + durable outbox
// retry preserve correctness across the release/re-acquire, and the source
// session is closed (bounded) on the failure path BEFORE the release so a standby
// never overlaps a still-subscribed old owner. The release is
// detached from ctx so it completes even if the caller is shutting down.
//
// wedged-Close guard: the release is CONDITIONAL on the bounded source
// Close actually completing. If Close ignored ctx and only the ceiling unblocked
// it (completed == false), the source is STILL subscribed; releasing the lease
// would let a standby acquire and overlap a still-consuming old owner — the exact
// split-brain exists to close. A session whose Close ignores ctx is
// unrecoverable in-process, so we do NOT release (the lease stays held and
// expires only by natural TTL, keeping single-owner) and instead escalate to a
// terminal ErrSessionUnrecoverable. superviseSession flips the runtime terminal;
// the pod restart forcibly tears down the wedged transport at the OS level
// (socket close on process exit), after which the standby takes over at TTL.
//
// A clean events-closed exit (err == nil) keeps the pre-existing re-acquire
// behaviour: it falls through to the lease-loss branch below.
func (m *Manager) afterRenewLoopExit(ctx context.Context, token persistence.LeaseToken, err error) error {
	if errors.Is(err, errStepDownCloseFailed) {
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
		return err
	}
	if err != nil && !errors.Is(err, errLeaseLostAfterRenewal) {
		m.pushLeaseEvent(LeaseStateReconcileFailed, token, err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
		// STOP consuming before the lease becomes releasable by a
		// standby. This branch is reached on a session failure (reconcile-on-
		// reconnect failure or an unexpected Events-channel close) while the
		// session is still connected/subscribed. Releasing the lease WITHOUT
		// closing the source first lets a standby acquire it while THIS owner's
		// route receiver keeps consuming+acking source messages — split-brain,
		// duplicate egress, source ACK by a non-owner. Close the source session
		// (bounded, so a wedged Close cannot hang the manager or stall a standby)
		// BEFORE releasing the lease.
		if _, closed := m.closeSourceBounded(ctx, m.releaseTimeout(), "session failure"); !closed {
			// Close ignored ctx and did not complete within the bounded
			// ceiling — the source is STILL subscribed. Do NOT release the lease
			// (that would re-open the split-brain); escalate to terminal so the
			// pod restart forcibly tears down the wedged transport. The lease
			// stays held and expires only by natural TTL, preserving single-owner
			// until the standby takes over.
			return fmt.Errorf("%w: source session close ignored ctx and did not complete on session-failure recovery: %w",
				ErrSessionUnrecoverable, err)
		}
		if errors.Is(err, ErrSessionUnrecoverable) {
			// A managed-cleanup quiescence timeout means previously accepted
			// route work may still mutate/send even though the broker socket is
			// disconnected. Do not transfer ownership underneath that work. The
			// supervisor marks the runtime terminal and cancellation stops
			// cooperative work; this lease expires naturally after process exit.
			return err
		}
		// Same hand-off hazard as a step-down, so the same bounded grace: the
		// close above fenced INGRESS, but destination sends already accepted
		// from this session may still be settling, and releasing straight away
		// lets the standby advance the fence underneath them.
		m.awaitSettlementGrace(ctx)
		m.releaseOwnedLeaseBestEffort(ctx, token, "session failure")
		return err
	}
	m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
	m.pushLeaseEvent(LeaseStateLost, token, err)
	m.mu.Lock()
	m.hasLease = false
	m.mu.Unlock()
	if errors.Is(err, errBrokerPathStepDown) {
		// The lease transferred (signals above are correct), but this process
		// must not compete for it again: see errBrokerPathStepDown.
		m.log(ctx, slog.LevelWarn,
			"lease released on broker-path step-down; escalating so this process restarts and rejoins as a standby",
			"error", err)
		return fmt.Errorf("%w: %w", ErrSessionUnrecoverable, err)
	}
	m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
	return nil
}
