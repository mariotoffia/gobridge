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
		// ev.Err is the transport's mapped CONNECT failure — the one place MQTT
		// says WHY a session cannot come back. Dropping it left readiness red
		// with no actionable cause in the runtime log at all. The level stays
		// Info: the preceding SessionDisconnected already raised a Warn, and the
		// adapter warns once per failed CONNECT with the bounded error code.
		m.log(ctx, slog.LevelInfo, "session reconnecting", "error", ev.Err)
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
	m.serving.Store(true)
	m.mu.Lock()
	m.notConvergedSince = time.Time{}
	m.mu.Unlock()
}

// markActivated records that a post-acquire activation completed, so from this
// instant the owner is EXPECTED to be serving and the broker-health outage clock
// is live for the rest of the term.
//
// It does not wait for a SessionConnected event. The transport's event channel
// drops its oldest entry under a storm, so an arming that waits for that event
// can miss it permanently and leave this owner renewing through an unbounded
// node-local broker outage no standby can take over.
//
// It does not wait for convergence either. A lease-bearing session with NO
// subscriptions — an exclusive egress session — has a Reconcile that issues no
// broker call at all and returns nil against a disconnected transport, so
// "the activation callback returned" proves nothing about connectivity. Waiting
// for a convergence that may never come would leave exactly that owner holding
// the partition forever. So the health of the source decides only WHICH state
// the term starts in: connected is converged, disconnected starts the outage
// clock here, at the moment serving was due.
func (m *Manager) markActivated(ctx context.Context) {
	if m.session.Health(ctx).Connected {
		m.markConverged()
		return
	}
	m.serving.Store(true)
	m.markNonConverged()
}

// beginBrokerPathTerm clears the per-term broker-path state at the START of a
// lease term, before its renewal loop runs.
//
// The outage clock describes ONE term's session. A term that inherited an armed
// timestamp from the previous one would find itself due on its very first renew
// tick — which fires while it is still connecting and reconciling — and step
// down on evidence about a session that no longer exists; the two loops would
// then trade the lease forever without either serving. The serving gate is
// per-term for the same reason: this term has not reached its activation yet, so
// a broker path that is down before then is activation, not an outage, and is
// bounded by the activation timeout instead.
func (m *Manager) beginBrokerPathTerm() {
	m.serving.Store(false)
	m.mu.Lock()
	m.notConvergedSince = time.Time{}
	m.mu.Unlock()
}

// markNonConverged starts the CLUSTER-2 broker-health step-down clock on the
// FIRST non-converged event after this term was due to be serving — before then
// (m.serving false) a broker path that is down is activation, not an OUTAGE, and
// is bounded separately by the activation timeout. It is idempotent: a run of
// disconnect/reconnecting/error events keeps the earliest timestamp so the
// elapsed non-converged time is measured from the outage start.
func (m *Manager) markNonConverged() {
	if m.brokerHealthStepDown <= 0 || !m.serving.Load() {
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
