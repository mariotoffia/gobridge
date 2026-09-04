package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// The renewal loop. It owns the lease for the whole term: it renews on its own
// timer, services session events between renewals, and is the single place a
// term decides it has lost ownership.

func (m *Manager) renewLoop(
	ctx context.Context,
	activationReady <-chan struct{},
	started chan<- struct{},
	cancelActivation context.CancelFunc,
) error {
	consecutiveFailures := 0
	var events <-chan ports.SessionEvent
	if activationReady == nil {
		events = m.session.Events()
	}

	timer := m.clk.NewTimer(m.nextRenewDelay())
	defer timer.Stop()
	close(started)

	stepDownForLoss := func() error {
		if activationReady == nil {
			return m.stepDown(ctx)
		}
		cancelActivation()
		token, closed := m.beginStepDown(ctx)
		return &activationLeaseLoss{token: token, closeCompleted: closed}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-activationReady:
			activationReady = nil
			events = m.session.Events()

		case ev, ok := <-events:
			if !ok {
				// Unexpected Events-channel close (ctx is still live): surface
				// it as a session failure so the one session is restarted,
				// instead of the old silent clean stop / false lease.lost.
				return fmt.Errorf("runtime: session-manager: %w", errSessionEventsClosed)
			}
			// bound the reconnect-driven Reconcile so a stalled SUBACK
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
			// authoritative Current read. The Current-read mitigation below
			// only applies BEFORE expiry; a write-failing/read-succeeding
			// partition keeps Current naming us past our real expiry, so relying
			// on it after the deadline is exactly what let an expired owner keep
			// consuming (~97s) alongside the standby that seizes at TTL.
			if m.leaseDeadlinePassed() {
				m.log(ctx, slog.LevelWarn,
					"local lease deadline reached; stepping down (fail-closed)")
				m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
				return stepDownForLoss()
			}

			// CLUSTER-2: node-local broker-path failover. The lease store is still
			// reachable (renewals below keep succeeding), but this owner's broker
			// path has stayed non-converged past the configured threshold — so a
			// standby cannot take over on lease loss alone. Step down voluntarily to
			// release the lease and let a healthy standby seize it. Opt-in
			// (brokerHealthStepDown > 0) so a globally-down broker does not churn the
			// lease between nodes that all fail to connect.
			if m.brokerHealthStepDownDue() {
				m.log(ctx, slog.LevelWarn,
					"broker path non-converged beyond broker_health_step_down; stepping down so a standby can take over",
					"threshold", m.brokerHealthStepDown)
				stepErr := stepDownForLoss()
				if stepErr == nil {
					// Impossible: every stepDownForLoss path returns non-nil.
					// Fail closed on the lease-loss shape rather than let a nil
					// fall through to the re-acquire this branch exists to stop.
					stepErr = errLeaseLostAfterRenewal
				}
				if !errors.Is(stepErr, errLeaseLostAfterRenewal) {
					// A source Close that ignored its context hands NOTHING over:
					// the lease is deliberately kept until natural expiry so no
					// standby can overlap a still-subscribed session. It is
					// already terminal, and it is NOT counted — the metric means
					// "an owner released its lease so a standby could take over",
					// which is what an operator alerts on.
					return stepErr
				}
				m.metrics.Counter(shared.MetricBrokerHealthStepDown, 1, leaseTag)
				return fmt.Errorf("%w: %w", errBrokerPathStepDown, stepErr)
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
				// owner.
				m.log(ctx, slog.LevelWarn, "lease definitively lost, stepping down immediately",
					"error", err)
				m.metrics.Counter(shared.MetricLeaseExpiries, 1, leaseTag)
				return stepDownForLoss()

			default:
				// Transient store error (timeout, throttling, unavailability):
				// tolerate up to MaxRenewFails before stepping down, so a brief
				// blip does not needlessly surrender the lease.
				consecutiveFailures++
				m.log(ctx, slog.LevelWarn, "lease renewal failed (transient)",
					"failures", consecutiveFailures, "error", err)
				if consecutiveFailures >= m.maxRenewFails {
					// before surrendering the lease on a run of TRANSIENT
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
						return stepDownForLoss()
					}
				}
			}

			timer.Reset(m.nextRenewDelay())
		}
	}
}
