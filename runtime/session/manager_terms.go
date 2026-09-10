package session

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// The two exclusive ownership loops. They differ only in WHEN the source
// session is connected: runExclusiveDeferred connects after the lease is won
// (connect_after_lease, the default for a route source, so a booting standby
// cannot resume a broker-persisted subscription without the lease), while
// runExclusive connects before Run and only reconciles per term.

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

		phase := "reconcile failure"
		escalatable := false
		term := m.runRenewingActivation(ctx, token, func(activationCtx context.Context) error {
			if !sessionStarted {
				phase = "deferred connect failure"
				// Nothing has been accepted in THIS Run: no subscription, no
				// delivery, no unsettled work. So a failure here is always safe
				// to release the just-acquired lease for, including the
				// permanent Start-after-Close refusal a single-use transport
				// returns when the supervisor restarts Run on a session a prior
				// term already closed. Retaining the lease there would leave a
				// provably dead owner holding a freshly re-seized row and delay
				// every standby to the full lease TTL instead of one poll.
				escalatable = true
				if err := m.session.Start(activationCtx); err != nil {
					return err
				}
				sessionStarted = true
			} else {
				phase = "deferred reconnect failure"
				escalatable = true
				if err := m.ensureConnected(activationCtx); err != nil {
					return err
				}
			}
			phase = "reconcile failure"
			escalatable = false
			return m.session.Reconcile(activationCtx, m.plan)
		})
		if term.terminalErr != nil {
			return term.terminalErr
		}
		if term.activationErr != nil {
			if term.activationDeadlineHandled {
				return term.activationErr
			}
			return m.releaseAndReturn(ctx, token, term.activationErr, phase, escalatable)
		}

		err = term.renewErr
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer.
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

		phase := "reconcile failure"
		escalatable := false
		term := m.runRenewingActivation(ctx, token, func(activationCtx context.Context) error {
			if reacquired {
				phase = "reconnect failure"
				escalatable = true
				if err := m.ensureConnected(activationCtx); err != nil {
					return err
				}
			}
			phase = "reconcile failure"
			escalatable = false
			return m.session.Reconcile(activationCtx, m.plan)
		})
		if term.terminalErr != nil {
			return term.terminalErr
		}
		if term.activationErr != nil {
			if term.activationDeadlineHandled {
				return term.activationErr
			}
			return m.releaseAndReturn(ctx, token, term.activationErr, phase, escalatable)
		}

		err = term.renewErr
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A genuine lease loss re-acquires (real transfer); a reconcile-on-
		// reconnect failure is surfaced for isolated restart instead of being
		// relabelled as a lease transfer.
		if propErr := m.afterRenewLoopExit(ctx, token, err); propErr != nil {
			return propErr
		}
	}
}
