package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Session-event handling for the non-exclusive path and the shared reconnect
// reconcile, plus the broker-health convergence clock that lets an owner whose
// broker path is down step aside for a healthy standby.

func (m *Manager) handleEvents(ctx context.Context) error {
	events := m.session.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// An unexpected Events-channel close (not driven by ctx
				// cancellation) means the underlying session died. Treat it as
				// a session FAILURE so superviseSession restarts this one
				// session in isolation, instead of the previous silent "clean
				// stop" that let a non-exclusive session die permanently with
				// no restart and no error.
				return fmt.Errorf("runtime: session-manager: %w", errSessionEventsClosed)
			}
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}
		}
	}
}

func (m *Manager) handleSessionEvent(ctx context.Context, ev ports.SessionEvent) error {
	sessionTag := shared.Tag{Key: shared.TagKeySessionID, Value: m.sessionID}
	switch ev.Type {
	case ports.SessionConnected:
		m.log(ctx, slog.LevelInfo, "session connected")
		if m.connectedOnce.Swap(true) {
			m.metrics.Counter(shared.MetricMQTTReconnects, 1, sessionTag)
		}
		if logging.DebugEnabled(m.logger) {
			m.logger.Log(ctx, logging.LevelDebug, "session reconcile",
				"session_id", m.sessionID,
				"subscription_count", len(m.plan.Subscriptions),
			)
		}
		// race the reconnect Reconcile against a hard ceiling so a broker
		// SDK call that ignores ctx cannot block the renew select loop and starve
		// lease renewal into a silent expiry + dual-consumer split-brain. The
		// renewal timer stays serviceable; on the ceiling the error flows through
		// the session-failure path, which closes the source session
		// before releasing the lease.
		if err := m.boundedReconcile(ctx, m.plan); err != nil {
			m.log(ctx, slog.LevelError, "reconcile failed on reconnect", "error", err)
			m.metrics.Counter(shared.MetricReconcileFailures, 1, sessionTag)
			// Still non-converged: leave the broker-health clock running.
			return fmt.Errorf("runtime: session-manager: reconcile on reconnect: %w", err)
		}
		// Connected AND re-subscribed: the broker path is converged again.
		m.markConverged()

	case ports.SessionDisconnected:
		m.log(ctx, slog.LevelWarn, "session disconnected", "error", ev.Err)
		m.markNonConverged()

	case ports.SessionReconnecting:
		m.log(ctx, slog.LevelInfo, "session reconnecting")
		m.markNonConverged()

	case ports.SessionError:
		m.log(ctx, slog.LevelError, "session error", "error", ev.Err)
		m.markNonConverged()
	}
	return nil
}

// markConverged records that the broker path is healthy (connected and
// re-subscribed), clearing the CLUSTER-2 broker-health step-down clock.
func (m *Manager) markConverged() {
	m.mu.Lock()
	m.notConvergedSince = time.Time{}
	m.mu.Unlock()
}

// markNonConverged starts the CLUSTER-2 broker-health step-down clock on the
// FIRST non-converged event after convergence, but only once this owner has been
// converged at least once (connectedOnce) — pre-first-connect activation is not a
// broker-path OUTAGE and is bounded separately by the activation timeout. It is
// idempotent: a run of disconnect/reconnecting/error events keeps the earliest
// timestamp so the elapsed non-converged time is measured from the outage start.
func (m *Manager) markNonConverged() {
	if m.brokerHealthStepDown <= 0 || !m.connectedOnce.Load() {
		return
	}
	m.mu.Lock()
	if m.notConvergedSince.IsZero() {
		m.notConvergedSince = m.clk.Now()
	}
	m.mu.Unlock()
}

// brokerHealthStepDownDue reports whether the broker path has stayed
// non-converged past the configured threshold, so the active owner should step
// down to let a standby take over a node-local broker outage (CLUSTER-2).
func (m *Manager) brokerHealthStepDownDue() bool {
	if m.brokerHealthStepDown <= 0 {
		return false
	}
	m.mu.Lock()
	since := m.notConvergedSince
	m.mu.Unlock()
	return !since.IsZero() && m.clk.Now().Sub(since) >= m.brokerHealthStepDown
}
