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
		m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
		m.pushLeaseEvent(LeaseStateLost, token, err)
		m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
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
		m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
		m.pushLeaseEvent(LeaseStateLost, token, err)
		m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
	}
}

func (m *Manager) acquireLeaseWithRetry(ctx context.Context) (persistence.LeaseToken, error) {
	leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}
	for {
		start := m.clk.Now()
		token, err := m.leaseStore.Acquire(ctx, m.sessionID, m.ownerID, m.leaseTTL, m.endpoints)
		m.metrics.Timer(shared.MetricLeaseAcquireLatency, m.clk.Since(start), leaseTag)
		if err == nil {
			return token, nil
		}
		m.metrics.Counter(shared.MetricLeaseAcquireFailures, 1, leaseTag)
		m.log(ctx, slog.LevelDebug, "lease acquisition failed, retrying", "error", err)

		select {
		case <-ctx.Done():
			return persistence.LeaseToken{}, ctx.Err()
		case <-m.clk.After(m.clampedInterval()):
		}
	}
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
				return nil
			}
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}

		case <-timer.C():
			m.mu.Lock()
			token := m.token
			m.mu.Unlock()

			leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}
			start := m.clk.Now()
			newToken, err := m.leaseStore.Renew(ctx, m.sessionID, token, m.leaseTTL, m.endpoints)
			m.metrics.Timer(shared.MetricLeaseRenewLatency, m.clk.Since(start), leaseTag)

			if err != nil {
				consecutiveFailures++
				m.log(ctx, slog.LevelWarn, "lease renewal failed",
					"failures", consecutiveFailures, "error", err)

				if consecutiveFailures >= m.maxRenewFails {
					m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
					return m.stepDown(ctx)
				}
			} else {
				consecutiveFailures = 0
				m.setToken(newToken)
				m.pushLeaseEvent(LeaseStateRenewed, newToken, nil)
				if logging.TraceEnabled(m.logger) {
					m.logger.Log(ctx, logging.LevelTrace, "lease renewed",
						"session_id", m.sessionID,
						"version", newToken.Version,
					)
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

	// Grace period must not be aborted by caller cancellation (we still
	// want in-flight work to settle), but we preserve trace/correlation
	// values via WithoutCancel.
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), m.stepDownGrace)
	defer graceCancel()

	select {
	case <-graceCtx.Done():
	case <-ctx.Done():
	}

	if m.leaseStore != nil {
		releaseTimeout := m.stepDownGrace
		if releaseTimeout <= 0 {
			releaseTimeout = 5 * time.Second
		} else if releaseTimeout > 5*time.Second {
			releaseTimeout = 5 * time.Second
		}
		// Release must complete even if caller ctx is cancelled, so we
		// detach cancellation but keep values.
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer releaseCancel()
		if err := m.leaseStore.Release(releaseCtx, m.sessionID, token); err != nil {
			m.emitLeaseAudit(ctx, "lease.release", "failure", token, err)
			m.pushLeaseEvent(LeaseStateReleased, token, err)
			m.log(ctx, slog.LevelWarn, "lease release failed during step-down", "error", err)
		} else {
			m.emitLeaseAudit(ctx, "lease.release", "success", token, nil)
			m.pushLeaseEvent(LeaseStateReleased, token, nil)
		}
	}

	return errors.New("lease lost after renewal failures")
}
