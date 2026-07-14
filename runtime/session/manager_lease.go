package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// errLeaseLostAfterRenewal is the sentinel stepDown returns once consecutive
// renewal failures cross MaxRenewFails. runExclusive/runExclusiveDeferred match
// it with errors.Is to distinguish a GENUINE lease loss (re-acquire — a real
// transfer) from any OTHER renewLoop exit, chiefly a reconcile-on-reconnect
// failure that must not masquerade as a lease transfer (C7-N2).
var errLeaseLostAfterRenewal = errors.New("lease lost after renewal failures")

// errSessionEventsClosed is returned when the session's Events channel closes
// WITHOUT the manager's context being cancelled — an unexpected death of the
// underlying session. It is surfaced (never treated as a clean stop or a lease
// loss) so superviseSession restarts the one session in isolation (finding
// L14).
var errSessionEventsClosed = errors.New("session events channel closed unexpectedly")

// ErrSessionUnrecoverable marks a lease-owning term that failed because the
// underlying session cannot be re-established IN THIS PROCESS. A single-use
// transport session (e.g. Paho MQTT) is closed by step-down; on the next
// lease re-acquisition ensureConnected cannot Start it again ("Start is not
// allowed after Close", surfaced as shared.ErrUnavailable wrapping the permanent
// shared.ErrTransportClosedPermanently marker) and no in-process rebuild path
// exists (the receiver/sender still reference the dead instance).
//
// superviseSession treats this as NON-recoverable-in-process and ESCALATES to
// terminal (documented process-restart backstop, scenario-08) instead of
// retrying the same dead session forever — the pre-fix behaviour that wedged the
// cluster: each capped-backoff retry re-Acquired via the store's same-owner
// fast path, bumped the lease version, and perpetually reset every standby's
// observation window (finding C3-CRITICAL). Ordinary permanently-closed restart
// paths release the lease before returning. Fail-closed managed migration is the
// exception: accepted route work or a broker-pinned delivery may remain, so the
// lease expires naturally while the orchestrator restarts the pod.
var ErrSessionUnrecoverable = errors.New("session cannot be re-established in this process")

// releaseAndReturn is the connect-failure recovery path (finding C3-CRITICAL /
// M12): a term acquired the lease but could not make the session usable
// (Start/ensureConnected/Reconcile failed while we hold the lease). It releases
// the just-acquired lease best-effort — otherwise a restarted Run would block in
// Acquire against our own unexpired lease until self-expiry, AND (on the
// single-use re-acquire path) each retry would re-seize via the store's
// same-owner fast path and fence out every standby forever.
//
// When escalatable is set (the re-acquire reconnect path) and the failure carries
// the permanent shared.ErrTransportClosedPermanently marker (a single-use session
// refusing Start-after-Close), the returned error is tagged ErrSessionUnrecoverable
// so superviseSession escalates to terminal rather than looping on the dead
// instance. All other failures (a first-connect broker blip, a transient reconcile
// rejection, a plain transient ErrUnavailable) are returned as-is for isolated
// capped-backoff retry. A non-escalatable permanent marker means managed
// migration already failed closed; it is made terminal without releasing the
// lease so ownership cannot transfer under unsettled work.
func (m *Manager) releaseAndReturn(ctx context.Context, token persistence.LeaseToken, err error, phase string, escalatable bool) error {
	m.mu.Lock()
	m.hasLease = false
	m.mu.Unlock()
	if errors.Is(err, shared.ErrTransportClosedPermanently) && !escalatable {
		// Managed migration failed closed after a broker-pinned delivery. The
		// transport already disconnected, but durable route work may still unwind;
		// preserve single ownership until natural TTL and force a fresh process.
		return fmt.Errorf("%w: %w", ErrSessionUnrecoverable, err)
	}
	m.releaseOwnedLeaseBestEffort(ctx, token, phase)
	if escalatesToUnrecoverable(err, escalatable) {
		return fmt.Errorf("%w: %w", ErrSessionUnrecoverable, err)
	}
	return err
}

// escalatesToUnrecoverable reports whether a connect failure on an escalatable
// path (the re-acquire reconnect, where the session was closed by a prior term's
// step-down) proves the transport instance is permanently closed in THIS process
// and must therefore escalate to a terminal ErrSessionUnrecoverable — releasing
// the lease and restarting the pod — rather than loop forever on the zombie.
//
// It gates strictly on the shared.ErrTransportClosedPermanently marker that
// single-use transports (paho/amqp091/amqp10) wrap into their Start-after-Close
// error, NOT the broad transient shared.ErrUnavailable. ErrUnavailable is also a
// transport's momentary "broker unreachable"; gating on it would turn a
// recoverable blip on a future MULTI-USE exclusive transport into a needless
// process restart, so those failures stay isolated capped-backoff retries. A
// first-connect failure (escalatable=false) never escalates: its Start hit a
// fresh, never-closed session, so it cannot carry the marker anyway.
func escalatesToUnrecoverable(err error, escalatable bool) bool {
	return escalatable && errors.Is(err, shared.ErrTransportClosedPermanently)
}

// isDefinitiveLeaseLoss reports whether a Renew error PROVES the lease is no
// longer ours (another owner has taken over, or the row is gone). These are the
// permanent fencing signals; the owner must step down IMMEDIATELY rather than
// burn MaxRenewFails renew intervals waiting — during which it would keep
// consuming alongside the new owner (finding H2). Transient store errors
// (timeouts, throttling, unavailability) are NOT definitive and still go
// through the consecutive-failure counter.
func isDefinitiveLeaseLoss(err error) bool {
	return errors.Is(err, shared.ErrStaleFencingToken) ||
		errors.Is(err, shared.ErrNotFound) ||
		errors.Is(err, shared.ErrVersionMismatch) ||
		errors.Is(err, shared.ErrAlreadyExists)
}

// withCallTimeout derives a per-call context bounding a single lease-store
// Acquire/Renew so a hung backend (e.g. a stalled DynamoDB request) cannot
// stretch step-down and takeover unboundedly (finding H3). The timeout is
// real-clock (context deadlines are not driven by the injected Clock); this is
// deliberate, as it bounds a genuinely-blocking I/O call.
func (m *Manager) withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.renewCallTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, m.renewCallTimeout)
}

// releaseTimeout bounds a best-effort lease Release.
func (m *Manager) releaseTimeout() time.Duration {
	t := m.stepDownGrace
	if t <= 0 || t > 5*time.Second {
		t = 5 * time.Second
	}
	return t
}

// releaseOwnedLeaseBestEffort releases the lease we still hold, detaching
// cancellation so the release completes even during shutdown, and emits the
// matching audit/observability signals. Used by stepDown and by the
// reconcile-failure/events-closed recovery path so a restarted Run re-acquires
// immediately instead of blocking in Acquire until LeaseTTL self-expiry
// (finding M12).
func (m *Manager) releaseOwnedLeaseBestEffort(ctx context.Context, token persistence.LeaseToken, reason string) {
	if m.leaseStore == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.releaseTimeout())
	defer cancel()
	if err := m.leaseStore.Release(releaseCtx, m.sessionID, token); err != nil {
		m.emitLeaseAudit(ctx, "lease.release", "failure", token, err)
		m.pushLeaseEvent(LeaseStateReleased, token, err)
		m.log(ctx, slog.LevelWarn, "lease release failed", "reason", reason, "error", err)
		return
	}
	m.emitLeaseAudit(ctx, "lease.release", "success", token, nil)
	m.pushLeaseEvent(LeaseStateReleased, token, nil)
	m.log(ctx, slog.LevelInfo, "lease released", "reason", reason)
}

func (m *Manager) runExclusiveDeferred(ctx context.Context) error {
	sessionStarted := false
	iteration := 0
	for {
		token, err := m.acquireLeaseWithRetry(ctx)
		if err != nil {
			return fmt.Errorf("runtime: session-manager: acquire lease: %w", err)
		}
		m.setToken(token)

		action := "lease.acquired"
		if iteration > 0 {
			action = "lease.reacquired"
			m.metrics.Counter(shared.MetricLeaseTransfers, 1,
				shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID})
		}
		iteration++
		m.emitLeaseAudit(ctx, action, "success", token, nil)
		m.pushLeaseEvent(LeaseStateAcquired, token, nil)

		m.log(ctx, slog.LevelInfo, "lease acquired (deferred connect)", "version", token.Version)

		if logging.DebugEnabled(m.logger) {
			m.logger.Log(ctx, logging.LevelDebug, "lease acquired",
				"session_id", m.sessionID,
				"owner_id", m.ownerID,
				"lease_version", token.Version,
			)
		}

		if !sessionStarted {
			if err := m.session.Start(ctx); err != nil {
				// First connect after acquiring the lease: a fresh session that
				// has not been closed yet. Release the lease and let
				// superviseSession retry in isolation (a broker blip is
				// transient); do NOT escalate to terminal.
				return m.releaseAndReturn(ctx, token, err, "deferred connect failure", false)
			}
			sessionStarted = true
		} else if err := m.ensureConnected(ctx); err != nil {
			// Re-acquire reconnect: the source session was closed by a prior
			// term's step-down. For a single-use session this Start cannot
			// succeed (it wraps ErrTransportClosedPermanently) — escalate to
			// terminal so the pod restarts with a fresh session instead of
			// looping on the zombie (finding C3-CRITICAL). Lease released either way.
			return m.releaseAndReturn(ctx, token, err, "deferred reconnect failure", true)
		}

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return m.releaseAndReturn(ctx, token, err, "reconcile failure", false)
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer (C7-N2).
		if propErr := m.afterRenewLoopExit(ctx, token, err); propErr != nil {
			return propErr
		}
	}
}

func (m *Manager) runExclusive(ctx context.Context) error {
	iteration := 0
	for {
		token, err := m.acquireLeaseWithRetry(ctx)
		if err != nil {
			return fmt.Errorf("runtime: session-manager: acquire lease: %w", err)
		}
		m.setToken(token)

		reacquired := iteration > 0
		action := "lease.acquired"
		if reacquired {
			action = "lease.reacquired"
			m.metrics.Counter(shared.MetricLeaseTransfers, 1,
				shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID})
		}
		iteration++
		m.emitLeaseAudit(ctx, action, "success", token, nil)
		m.pushLeaseEvent(LeaseStateAcquired, token, nil)

		m.log(ctx, slog.LevelInfo, "lease acquired", "version", token.Version)

		if logging.DebugEnabled(m.logger) {
			m.logger.Log(ctx, logging.LevelDebug, "lease acquired",
				"session_id", m.sessionID,
				"owner_id", m.ownerID,
				"lease_version", token.Version,
			)
		}

		if reacquired {
			// A previous term's stepDown closed the source session to stop a
			// non-owner from consuming/ACKing source messages. Now that we own
			// the lease again, re-establish the session before reconciling. For
			// a single-use session this Start cannot succeed (it wraps
			// ErrTransportClosedPermanently); escalate to terminal so the pod
			// restarts with a fresh session instead of looping on the zombie,
			// releasing the lease so a healthy standby takes over immediately
			// (finding C3-CRITICAL).
			if err := m.ensureConnected(ctx); err != nil {
				return m.releaseAndReturn(ctx, token, err, "reconnect failure", true)
			}
		}

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return m.releaseAndReturn(ctx, token, err, "reconcile failure", false)
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer (C7-N2).
		if propErr := m.afterRenewLoopExit(ctx, token, err); propErr != nil {
			return propErr
		}
	}
}

func (m *Manager) acquireLeaseWithRetry(ctx context.Context) (persistence.LeaseToken, error) {
	leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}
	// Rate-limited escalation for a lease-store OUTAGE (finding L9). Normal
	// standby contention (another live owner) is expected and stays quiet; only
	// genuine store failures escalate Warn -> Error so an outage is not silent.
	const (
		acquireEscalateAfter = 5                // consecutive outage failures before Error
		acquireWarnEvery     = 15 * time.Second // minimum spacing between outage logs
	)
	var (
		outageFailures int
		firstOutage    time.Time
		lastOutageLog  time.Time
	)
	for {
		callCtx, cancel := m.withCallTimeout(ctx)
		start := m.clk.Now()
		token, err := m.leaseStore.Acquire(callCtx, m.sessionID, m.ownerID, m.leaseTTL, m.endpoints)
		cancel()
		m.metrics.Timer(shared.MetricLeaseAcquireLatency, m.clk.Since(start), leaseTag)
		if err == nil {
			// Record the local lease deadline from the PRE-call timestamp so it
			// is at or before the store's authoritative ExpiresAt (fail-closed).
			m.recordLeaseDeadline(start)
			if outageFailures > 0 {
				m.log(ctx, slog.LevelInfo, "lease store recovered, lease acquired",
					"after_failures", outageFailures)
			}
			return token, nil
		}
		m.metrics.Counter(shared.MetricLeaseAcquireFailures, 1, leaseTag)

		if isExpectedAcquireContention(err) {
			// Another instance still owns the (unexpired) lease. This is the
			// normal standby wait, not an outage: keep it at Debug and reset the
			// outage escalation — the store is clearly reachable.
			outageFailures = 0
			firstOutage = time.Time{}
			lastOutageLog = time.Time{}
			m.log(ctx, slog.LevelDebug, "lease held by another owner, awaiting expiry", "error", err)
		} else {
			outageFailures++
			now := m.clk.Now()
			if firstOutage.IsZero() {
				firstOutage = now
			}
			level := slog.LevelWarn
			if outageFailures >= acquireEscalateAfter || now.Sub(firstOutage) >= m.leaseTTL {
				level = slog.LevelError
			}
			if lastOutageLog.IsZero() || now.Sub(lastOutageLog) >= acquireWarnEvery {
				m.log(ctx, level, "lease store acquire failing; standby cannot take over",
					"failures", outageFailures,
					"elapsed_ms", now.Sub(firstOutage).Milliseconds(),
					"error", err)
				lastOutageLog = now
			}
		}

		// Standbys poll on a DEDICATED, faster cadence than owners renew, so a
		// takeover is not delayed by up to a full renew interval (finding M6).
		select {
		case <-ctx.Done():
			return persistence.LeaseToken{}, ctx.Err()
		case <-m.clk.After(m.acquirePollDelay()):
		}
	}
}

// isExpectedAcquireContention reports whether an Acquire error just means the
// lease is currently held by another live owner — the normal standby-poll
// outcome, not a store outage.
func isExpectedAcquireContention(err error) bool {
	return errors.Is(err, shared.ErrAlreadyExists)
}

// eventReconcileTimeout bounds a reconnect-driven Reconcile invoked from the
// renew loop. A single-owner MQTT session re-runs Reconcile (re-Subscribe) on
// the caller's context after every reconnect; a broker that answers keepalive
// but stalls SUBACK would otherwise block the renew select loop indefinitely,
// so the renew timer.C() case is never selected and the lease silently expires
// while the broker-resumed subscription keeps delivering to this now-non-owner
// alongside the new owner — two live consumers for unbounded time (finding F1).
//
// Bounding the call at min(RenewCallTimeout, LeaseTTL/4) caps how long the loop
// can be blocked so the renew timer still fires before expiry; a hung Reconcile
// times out and its error is surfaced on the existing session-failure path
// (afterRenewLoopExit releases the lease and the session restarts) rather than
// continuing blind. LeaseTTL/4 is only the CEILING — in practice RenewCallTimeout
// (default min(RenewInterval/2, 5s)) is the smaller term and thus the effective
// bound. That is deliberate: keeping the reconcile bound well under LeaseTTL/4
// preserves the renew-expiry margin even for a large-RenewInterval,
// MaxRenewFails=1 config, where a full LeaseTTL/4 stall could push the next
// renewal past expiry. The cost is that a legitimately slow re-Subscribe (many
// filters / laggy broker) exceeding RenewCallTimeout is cut off and the session
// restarts rather than eventually succeeding — a bounded, observable degradation
// chosen over the silent lease-expiry + dual-consumer hazard this bound prevents.
// Operators raising RenewCallTimeout should note it also relaxes this bound. The
// bound relies on the transport honoring context cancellation (paho's
// ConnectionManager Subscribe/Unsubscribe do); a transport that ignores ctx
// cannot be unblocked this way.
func (m *Manager) eventReconcileTimeout() time.Duration {
	d := m.leaseTTL / 4
	if m.renewCallTimeout > 0 && m.renewCallTimeout < d {
		d = m.renewCallTimeout
	}
	return d
}

// eventReconcileContext derives the per-event context that bounds a
// reconnect-driven Reconcile (see eventReconcileTimeout). It mirrors
// withCallTimeout: a non-positive bound yields a plain cancellable child so the
// caller can always cancel() unconditionally.
func (m *Manager) eventReconcileContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if d := m.eventReconcileTimeout(); d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

// leaseStillHeld does one authoritative Current read to decide whether a run of
// TRANSIENT renew failures actually cost us the lease (finding F7). It returns
// true ONLY when the store is reachable AND still names us as the unexpired
// owner. A Current error (store unreachable) or an other-owner / expired /
// absent row returns false: ownership is then lost or unverifiable, and the
// exclusive-safety posture is to step down (fail-closed), consistent with the
// locator's ownership-unknown default. The read reuses the per-call timeout so
// a hung store cannot stretch step-down. m.ownerID is immutable after
// construction, so this is safe to call from the renew loop without locking.
func (m *Manager) leaseStillHeld(ctx context.Context) bool {
	callCtx, cancel := m.withCallTimeout(ctx)
	info, err := m.leaseStore.Current(callCtx, m.sessionID)
	cancel()
	if err != nil {
		return false
	}
	return info.Owner == m.ownerID && m.clk.Now().Before(info.ExpiresAt)
}

func (m *Manager) renewLoop(ctx context.Context) error {
	consecutiveFailures := 0
	events := m.session.Events()

	timer := m.clk.NewTimer(m.nextRenewDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-events:
			if !ok {
				// Unexpected Events-channel close (ctx is still live): surface
				// it as a session failure so the one session is restarted,
				// instead of the old silent clean stop / false lease.lost
				// (finding L14).
				return fmt.Errorf("runtime: session-manager: %w", errSessionEventsClosed)
			}
			// F1: bound the reconnect-driven Reconcile so a stalled SUBACK
			// cannot block this select and starve lease renewal. On timeout the
			// wrapped ctx error propagates and the session is cleanly restarted.
			// cancel() is called explicitly (not deferred) so the per-event
			// contexts do not accumulate across a long-lived renew loop.
			evCtx, cancel := m.eventReconcileContext(ctx)
			err := m.handleSessionEvent(evCtx, ev)
			cancel()
			if err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}

		case <-timer.C():
			leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}

			// ROOT-CAUSE fail-closed gate (split-brain renew-fail/read-succeed):
			// once our own lease deadline (last successful acquire/renew + TTL)
			// has passed, step down UNCONDITIONALLY — before any renew attempt or
			// authoritative Current read. The F7 Current-read mitigation below
			// only applies BEFORE expiry; a write-failing/read-succeeding
			// partition keeps Current naming us past our real expiry, so relying
			// on it after the deadline is exactly what let an expired owner keep
			// consuming (~97s) alongside the standby that seizes at TTL.
			if m.leaseDeadlinePassed() {
				m.log(ctx, slog.LevelWarn,
					"local lease deadline reached; stepping down (fail-closed)")
				m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
				return m.stepDown(ctx)
			}

			m.mu.Lock()
			token := m.token
			m.mu.Unlock()

			callCtx, cancel := m.withCallTimeout(ctx)
			start := m.clk.Now()
			newToken, err := m.leaseStore.Renew(callCtx, m.sessionID, token, m.leaseTTL, m.endpoints)
			cancel()
			m.metrics.Timer(shared.MetricLeaseRenewLatency, m.clk.Since(start), leaseTag)

			switch {
			case err == nil:
				consecutiveFailures = 0
				m.setToken(newToken)
				// Extend the local deadline from the PRE-call timestamp (start)
				// so it stays at or before the store's authoritative ExpiresAt.
				m.recordLeaseDeadline(start)
				m.pushLeaseEvent(LeaseStateRenewed, newToken, nil)
				if logging.TraceEnabled(m.logger) {
					m.logger.Log(ctx, logging.LevelTrace, "lease renewed",
						"session_id", m.sessionID,
						"version", newToken.Version,
					)
				}

			case isDefinitiveLeaseLoss(err):
				// The lease is provably no longer ours (a new owner took over or
				// the row is gone). Step down NOW rather than waiting out
				// MaxRenewFails renew intervals while consuming alongside the new
				// owner (finding H2).
				m.log(ctx, slog.LevelWarn, "lease definitively lost, stepping down immediately",
					"error", err)
				m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
				return m.stepDown(ctx)

			default:
				// Transient store error (timeout, throttling, unavailability):
				// tolerate up to MaxRenewFails before stepping down, so a brief
				// blip does not needlessly surrender the lease.
				consecutiveFailures++
				m.log(ctx, slog.LevelWarn, "lease renewal failed (transient)",
					"failures", consecutiveFailures, "error", err)
				if consecutiveFailures >= m.maxRenewFails {
					// F7: before surrendering the lease on a run of TRANSIENT
					// failures, do ONE authoritative Current read. A transient
					// store blip that never actually cost us the lease would
					// otherwise force a step-down — and for a single-use MQTT
					// owner that means a process restart, turning a brief store
					// wobble (e.g. a DynamoDB throttle) into a fleet-wide
					// reconnect herd. If the read still shows us as the live
					// owner, treat the streak as a no-op and keep renewing; a
					// Current error or any other-owner/expired row is treated as
					// loss (fail-closed, per the exclusive-safety posture).
					//
					// This mitigation is now bounded ABOVE by the local lease
					// deadline: it can only continue renewing while we are still
					// BEFORE our own expiry (leaseDeadlinePassed forces an
					// unconditional step-down at the top of this case once the
					// deadline is reached). A write-failing/read-succeeding
					// partition therefore no longer keeps an EXPIRED owner active
					// on the strength of Current reads (split-brain fix).
					if m.leaseStillHeld(ctx) {
						m.log(ctx, slog.LevelWarn,
							"lease renewal failing but authoritative read still shows us as owner; not stepping down",
							"failures", consecutiveFailures)
						// Re-arm to one BELOW the threshold, not zero, so the next
						// failed renew re-runs the authoritative Current check
						// instead of granting a fresh full MaxRenewFails budget.
						// During a sustained write-failing / read-succeeding
						// partition this keeps re-checking each renew interval; the
						// hard past-expiry bound is now the leaseDeadline gate
						// above, which fails closed at TTL regardless of Current.
						// The window stays fenced on the data path (no loss / no
						// double-commit). maxRenewFails is floored to 1 at
						// construction, so this is >= 0.
						consecutiveFailures = m.maxRenewFails - 1
					} else {
						m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
						return m.stepDown(ctx)
					}
				}
			}

			timer.Reset(m.nextRenewDelay())
		}
	}
}

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

	// Stop accepting source messages now that we no longer hold the lease.
	// A stepped-down owner must not keep consuming or ACKing from the source
	// while a new owner takes over (split-brain). In the common case source and
	// destination are distinct sessions, so closing the source does not abort
	// the grace drain below: that drain settles in-flight outbox Send+Complete
	// on the destination side, which does not need the source connection. A
	// source ACK lost on close is redelivered to the new owner (at-least-once;
	// see Config.StepDownGrace docs).
	//
	// Caveat: in a same-broker MQTT->MQTT topology the factory can hand the same
	// *Session to both receiver and sender (paho/factory.go), so closing it also
	// closes the destination publish path. There the grace drain cannot complete
	// in-process; in-flight outbox records instead settle via the durable,
	// fencing-protected retry path after the next acquisition. Still no loss —
	// only the in-grace settlement optimization is forfeited for that topology.
	if err := m.session.Close(ctx); err != nil {
		m.log(ctx, slog.LevelWarn, "source session close failed during step-down", "error", err)
	}

	// Grace period must NOT be aborted by caller cancellation. Its contract
	// (Config.StepDownGrace) is to give in-flight outbox Send+Complete a full
	// settle window; aborting it on shutdown leaves work the new owner re-sends
	// as a fenced duplicate. Run it on a detached (WithoutCancel) timer and wait
	// it out unconditionally — reaching stepDown already means the lease is lost,
	// not that the caller is shutting down (a shutdown cancels renewLoop via its
	// ctx.Done() case before any renewal-failure step-down) (finding M13).
	//
	// F9: skip the wait entirely when the destination drainer reports idle — no
	// claimable pending outbox work remains to settle, so the grace would add
	// only takeover latency. "Idle" means no CLAIMABLE pending records, not
	// "nothing in flight at the store": a record already claimed whose Send
	// succeeded but Complete failed is momentarily invisible to the idle check
	// and is re-sent (fenced) by the new owner after its claim lapses. That, and
	// a source message ACKed just after this check landing in the outbox (stale
	// idle), both settle via the new owner's fenced retry — within the documented
	// at-least-once window (Config.StepDownGrace / AckBoundary): no loss, no
	// double-commit.
	if m.drainIdle == nil || !m.drainIdle() {
		graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), m.stepDownGrace)
		defer graceCancel()
		<-graceCtx.Done()
	}

	m.releaseOwnedLeaseBestEffort(ctx, token, "step-down")

	return errLeaseLostAfterRenewal
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
//     lease-transfer observability uncontaminated (C7-N2). hasLease is cleared
//     so a drainer does not act on a lease this failing session is no longer
//     renewing during the restart window.
//
// Release-then-reacquire recovery (finding M12): on a session failure the
// still-held lease is RELEASED best-effort here before the restart, so the
// restarted Run re-acquires immediately instead of blocking in Acquire against
// our own unexpired lease until LeaseTTL self-expiry. Fencing + durable outbox
// retry preserve correctness across the release/re-acquire, and the source
// session is closed (bounded) on the failure path BEFORE the release so a standby
// never overlaps a still-subscribed old owner (CRITICAL-1). The release is
// detached from ctx so it completes even if the caller is shutting down.
//
// B1 — wedged-Close guard: the M12 release is CONDITIONAL on the bounded source
// Close actually completing. If Close ignored ctx and only the ceiling unblocked
// it (completed == false), the source is STILL subscribed; releasing the lease
// would let a standby acquire and overlap a still-consuming old owner — the exact
// split-brain CRITICAL-1 exists to close. A session whose Close ignores ctx is
// unrecoverable in-process, so we do NOT release (the lease stays held and
// expires only by natural TTL, keeping single-owner) and instead escalate to a
// terminal ErrSessionUnrecoverable. superviseSession flips the runtime terminal;
// the pod restart forcibly tears down the wedged transport at the OS level
// (socket close on process exit), after which the standby takes over at TTL.
//
// A clean events-closed exit (err == nil) keeps the pre-existing re-acquire
// behaviour: it falls through to the lease-loss branch below.
func (m *Manager) afterRenewLoopExit(ctx context.Context, token persistence.LeaseToken, err error) error {
	if err != nil && !errors.Is(err, errLeaseLostAfterRenewal) {
		m.pushLeaseEvent(LeaseStateReconcileFailed, token, err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
		// CRITICAL-1: STOP consuming before the lease becomes releasable by a
		// standby. This branch is reached on a session failure (reconcile-on-
		// reconnect failure or an unexpected Events-channel close) while the
		// session is still connected/subscribed. Releasing the lease WITHOUT
		// closing the source first lets a standby acquire it while THIS owner's
		// route receiver keeps consuming+acking source messages — split-brain,
		// duplicate egress, source ACK by a non-owner. Close the source session
		// (bounded, so a wedged Close cannot hang the manager or stall a standby)
		// BEFORE releasing the lease.
		if !m.closeSourceBounded(ctx, "session failure") {
			// B1: Close ignored ctx and did not complete within the bounded
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
		m.releaseOwnedLeaseBestEffort(ctx, token, "session failure")
		return err
	}
	m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
	m.pushLeaseEvent(LeaseStateLost, token, err)
	m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
	m.mu.Lock()
	m.hasLease = false
	m.mu.Unlock()
	return nil
}
