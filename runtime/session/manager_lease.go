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
				return err
			}
			sessionStarted = true
		} else if err := m.ensureConnected(ctx); err != nil {
			return err
		}

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return err
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer (C7-N2).
		if m.afterRenewLoopExit(ctx, token, err) {
			return err
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
			// the lease again, re-establish the session before reconciling.
			if err := m.ensureConnected(ctx); err != nil {
				return err
			}
		}

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return err
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer (C7-N2).
		if m.afterRenewLoopExit(ctx, token, err) {
			return err
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

func (m *Manager) renewLoop(ctx context.Context) error {
	consecutiveFailures := 0
	events := m.session.Events()

	timer := m.clk.NewTimer(m.clampedInterval())
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
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}

		case <-timer.C():
			m.mu.Lock()
			token := m.token
			m.mu.Unlock()

			leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}
			callCtx, cancel := m.withCallTimeout(ctx)
			start := m.clk.Now()
			newToken, err := m.leaseStore.Renew(callCtx, m.sessionID, token, m.leaseTTL, m.endpoints)
			cancel()
			m.metrics.Timer(shared.MetricLeaseRenewLatency, m.clk.Since(start), leaseTag)

			switch {
			case err == nil:
				consecutiveFailures = 0
				m.setToken(newToken)
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
					m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
					return m.stepDown(ctx)
				}
			}

			timer.Reset(m.clampedInterval())
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
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), m.stepDownGrace)
	defer graceCancel()
	<-graceCtx.Done()

	m.releaseOwnedLeaseBestEffort(ctx, token, "step-down")

	return errLeaseLostAfterRenewal
}

// afterRenewLoopExit classifies renewLoop's non-ctx return and emits the
// matching observability signal. It reports whether the caller must PROPAGATE
// the error (return from Run) rather than re-acquire the lease.
//
//   - Genuine lease loss (stepDown crossed the renewal-failure threshold or a
//     definitive fencing signal, errLeaseLostAfterRenewal): emit lease.lost +
//     LeaseStateLost, clear hasLease, and return false so the caller
//     re-acquires. This — and only this — is a real lease transfer
//     (MetricLeaseTransfers on re-acquire). stepDown has already released the
//     lease, so the re-acquire is unimpeded.
//
//   - Session failure (any other non-nil err: a reconcile-on-reconnect failure
//     or an unexpected Events-channel close): the lease is still held and
//     unexpired, so this is NOT a lease loss. Emit the DISTINCT
//     LeaseStateReconcileFailed signal (never lease.lost / LeaseStateLost / a
//     MetricLeaseTransfers-bearing re-acquire — MetricReconcileFailures was
//     already emitted at the failure site) and return true so the caller
//     surfaces the error to superviseSession for isolated restart, keeping
//     lease-transfer observability uncontaminated (C7-N2). hasLease is cleared
//     so a drainer does not act on a lease this failing session is no longer
//     renewing during the restart window.
//
// Release-then-reacquire recovery (finding M12): the still-held lease is
// RELEASED best-effort here before the restart. Previously it was deliberately
// left in place, forcing superviseSession's restarted Run to block in Acquire
// against our own unexpired lease until LeaseTTL self-expiry (up to 360s with
// defaults) — a needless self-inflicted outage. Releasing first lets the
// restart re-acquire immediately; fencing + durable outbox retry preserve
// correctness across the release/re-acquire, and the source session was already
// closed on the failure path. The release is detached from ctx so it completes
// even if the caller is shutting down.
//
// A clean events-closed exit (err == nil) keeps the pre-existing re-acquire
// behaviour: it falls through to the lease-loss branch below.
func (m *Manager) afterRenewLoopExit(ctx context.Context, token persistence.LeaseToken, err error) bool {
	if err != nil && !errors.Is(err, errLeaseLostAfterRenewal) {
		m.pushLeaseEvent(LeaseStateReconcileFailed, token, err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
		m.releaseOwnedLeaseBestEffort(ctx, token, "session failure")
		return true
	}
	m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
	m.pushLeaseEvent(LeaseStateLost, token, err)
	m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
	m.mu.Lock()
	m.hasLease = false
	m.mu.Unlock()
	return false
}
