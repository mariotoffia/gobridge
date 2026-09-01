package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// errLeaseLostAfterRenewal is the sentinel stepDown returns once consecutive
// renewal failures cross MaxRenewFails. runExclusive/runExclusiveDeferred match
// it with errors.Is to distinguish a GENUINE lease loss (re-acquire — a real
// transfer) from any OTHER renewLoop exit, chiefly a reconcile-on-reconnect
// failure that must not masquerade as a lease transfer.
var errLeaseLostAfterRenewal = errors.New("lease lost after renewal failures")

// errSessionEventsClosed is returned when the session's Events channel closes
// WITHOUT the manager's context being cancelled — an unexpected death of the
// underlying session. It is surfaced (never treated as a clean stop or a lease
// loss) so superviseSession restarts the one session in isolation (finding
// L14).
var errSessionEventsClosed = errors.New("session events channel closed unexpectedly")
var errStepDownCloseFailed = errors.New("source session close failed during lease step-down")

// activationLeaseLoss records that the existing renewal loop detected lease
// loss while initial activation was still running. In that phase the loop
// cancels activation and disconnects immediately, but defers Release until the
// activation callback is proved settled.
type activationLeaseLoss struct {
	token          persistence.LeaseToken
	closeCompleted bool
}

func (*activationLeaseLoss) Error() string { return "lease lost during post-acquire activation" }
func (*activationLeaseLoss) Unwrap() error { return errLeaseLostAfterRenewal }

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
// observation window. Ordinary permanently-closed restart
// paths release the lease before returning. Fail-closed managed migration is the
// exception: accepted route work or a broker-pinned delivery may remain, so the
// lease expires naturally while the orchestrator restarts the pod.
var ErrSessionUnrecoverable = errors.New("session cannot be re-established in this process")

// releaseAndReturn is the connect-failure recovery path (finding /
// M12): a term acquired the lease but could not make the session usable
// (Start/ensureConnected/Reconcile failed while we hold the lease). It releases
// the just-acquired lease best-effort — otherwise a restarted Run would block in
// Acquire against our own unexpired lease until self-expiry, AND (on the
// single-use re-acquire path) each retry would re-seize via the store's
// same-owner fast path and fence out every standby forever.
//
// escalatable marks the phases where NOTHING has been accepted yet — the
// deferred connect, and the re-acquire reconnect — so releasing is
// unconditionally safe. When it is set and the failure carries the permanent
// shared.ErrTransportClosedPermanently marker (a single-use session refusing
// Start-after-Close), the lease is released AND the returned error is tagged
// ErrSessionUnrecoverable, so superviseSession escalates to terminal rather than
// looping on the dead instance while a standby waits out the TTL. All other
// failures (a broker blip, a transient reconcile rejection, a plain transient
// ErrUnavailable) are returned as-is for isolated capped-backoff retry. A
// non-escalatable permanent marker is the reconcile / managed-migration phase:
// migration already failed closed and durable route work may still unwind, so it
// is made terminal WITHOUT releasing the lease and ownership cannot transfer
// under unsettled work.
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
	if escalatesToUnrecoverable(err, escalatable) {
		// About to hand ownership to a standby on the strength of "this transport
		// is permanently closed, so nothing of ours can still send". Prove it
		// rather than infer it: a session may latch its permanent marker
		// ASYNCHRONOUSLY — the paho session's ingress-poison rejection returns
		// immediately and quiesces on a goroutine — so a Start that reports the
		// marker does NOT by itself mean accepted deliveries have stopped
		// settling. Close the source (bounded) first and keep the lease when that
		// close did not complete, exactly as the session-failure path does. In
		// the ordinary case (a session an earlier term already closed) this is a
		// no-op that returns at once.
		if _, closed := m.closeSourceBounded(ctx, m.releaseTimeout(), phase); !closed {
			return fmt.Errorf("%w: source session close did not complete before handing off after %s: %w",
				ErrSessionUnrecoverable, phase, err)
		}
		m.releaseOwnedLeaseBestEffort(ctx, token, phase)
		return fmt.Errorf("%w: %w", ErrSessionUnrecoverable, err)
	}
	m.releaseOwnedLeaseBestEffort(ctx, token, phase)
	return err
}

// escalatesToUnrecoverable reports whether a connect failure on an escalatable
// path (the deferred connect or the re-acquire reconnect, where the session may
// have been closed by a prior term's step-down or session-failure recovery)
// proves the transport instance is permanently closed in THIS process and must
// therefore escalate to a terminal ErrSessionUnrecoverable — releasing the lease
// and restarting the pod — rather than loop forever on the zombie.
//
// It gates strictly on the shared.ErrTransportClosedPermanently marker that
// single-use transports (paho/amqp091/amqp10) wrap into their Start-after-Close
// error, NOT the broad transient shared.ErrUnavailable. ErrUnavailable is also a
// transport's momentary "broker unreachable"; gating on it would turn a
// recoverable blip on a future MULTI-USE exclusive transport into a needless
// process restart, so those failures stay isolated capped-backoff retries. The
// very first connect of a process cannot carry the marker anyway — its Start
// hits a fresh, never-closed session — so the marker only ever fires on a Run
// that inherited a session an earlier term already closed.
func escalatesToUnrecoverable(err error, escalatable bool) bool {
	return escalatable && errors.Is(err, shared.ErrTransportClosedPermanently)
}

// isDefinitiveLeaseLoss reports whether a Renew error PROVES the lease is no
// longer ours (another owner has taken over, or the row is gone). These are the
// permanent fencing signals; the owner must step down IMMEDIATELY rather than
// burn MaxRenewFails renew intervals waiting — during which it would keep
// consuming alongside the new owner. Transient store errors
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
// stretch step-down and takeover unboundedly. The timeout is
// real-clock (context deadlines are not driven by the injected Clock); this is
// deliberate, as it bounds a genuinely-blocking I/O call.
func (m *Manager) withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.renewCallTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, m.renewCallTimeout)
}

// boundedReleaseTimeout is the shared teardown/release margin used by both
// pre-build timing validation and the live manager.
func boundedReleaseTimeout(stepDownGrace time.Duration) time.Duration {
	if stepDownGrace <= 0 || stepDownGrace > 5*time.Second {
		return 5 * time.Second
	}
	return stepDownGrace
}

// releaseTimeout bounds a best-effort lease Release and source teardown.
func (m *Manager) releaseTimeout() time.Duration {
	return boundedReleaseTimeout(m.stepDownGrace)
}

// releaseOwnedLeaseBestEffort releases the lease we still hold, detaching
// cancellation so the release completes even during shutdown, and emits the
// matching audit/observability signals. Used by stepDown and by the
// reconcile-failure/events-closed recovery path so a restarted Run re-acquires
// immediately instead of blocking in Acquire until LeaseTTL self-expiry.
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
