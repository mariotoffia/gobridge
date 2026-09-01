package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The Manager's outward lifecycle and control surface: Run's mode dispatch, the
// pre-Run setters, the lease-transition event channel, and Close.

// Run starts the session and manages its lifecycle. For exclusive sessions,
// it acquires the lease and runs the renewal loop. It blocks until ctx is
// cancelled or an unrecoverable error occurs.
//
// When ConnectAfterLease is set, the session is not started until the
// lease has been acquired, preventing premature broker connections that
// would displace the current owner.
func (m *Manager) Run(ctx context.Context) (retErr error) {
	defer func() {
		if retErr != nil && errors.Is(retErr, shared.ErrTransportClosedPermanently) && !errors.Is(retErr, ErrSessionUnrecoverable) {
			retErr = fmt.Errorf("%w: %w", ErrSessionUnrecoverable, retErr)
		}
	}()
	if m.exclusive && m.leaseStore != nil && m.connectAfterLease {
		return m.runExclusiveDeferred(ctx)
	}

	if err := m.session.Start(ctx); err != nil {
		return err
	}

	if m.exclusive && m.leaseStore != nil {
		return m.runExclusive(ctx)
	}

	if err := m.session.Reconcile(ctx, m.plan); err != nil {
		return err
	}

	return m.handleEvents(ctx)
}

// SetMetrics sets the metrics exporter on the manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *Manager) SetMetrics(metrics ports.MetricsExporter) {
	m.metrics = metrics
}

// SetAudit sets the audit logger on the manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *Manager) SetAudit(audit ports.AuditLogger) {
	m.audit = audit
}

// SetEndpoints sets the cluster endpoints that identify the local
// instance to peers. Must be called before Run; not safe for concurrent
// use with Run.
func (m *Manager) SetEndpoints(endpoints map[string]string) {
	m.endpoints = endpoints
}

// SetDrainIdleCheck installs an optional predicate that reports whether the
// destination outbox drainer for this session is idle (no in-flight records
// left to settle). stepDown consults it to early-complete the StepDownGrace
// wait: when there is nothing in flight, waiting the full grace only adds
// takeover latency (a new owner keys off the lease store, not this wait).
// When unset (nil), stepDown waits the full grace as before.
// Must be called before Run; not safe for concurrent use with Run. The
// predicate itself may be invoked concurrently with the drainer and must be
// safe for that (Drainer.IdleSince is).
func (m *Manager) SetDrainIdleCheck(fn func() bool) {
	m.drainIdle = fn
}

// leaseEventBuffer is the capacity of the LeaseStateChanged channel. Lease
// transitions are low-frequency but observability-critical; the buffer is sized
// so a slow consumer rarely fills it, and pushLeaseEvent coalesces on overflow
// rather than silently dropping.
const leaseEventBuffer = 64

// LeaseStateChanged returns a channel that receives lease state transitions.
func (m *Manager) LeaseStateChanged() <-chan LeaseStateEvent { return m.leaseEvents }

// LeaseEventDrops returns the number of lease state transitions that had to
// overwrite an older, still-unconsumed event because the buffer was full. A
// non-zero value means a consumer is not draining LeaseStateChanged promptly;
// the newest transitions are always preserved.
func (m *Manager) LeaseEventDrops() uint64 { return m.leaseEventDrops.Load() }

// pushLeaseEvent delivers a lease transition to LeaseStateChanged. On a full
// buffer it does NOT silently drop the newest event: it evicts the OLDEST
// buffered event (the least relevant — state has moved on since) and enqueues
// the new one, incrementing a drop counter for observability. The current
// state is more valuable to a consumer than a stale one, so overwrite-oldest
// preserves the freshest transitions.
func (m *Manager) pushLeaseEvent(state LeaseState, token persistence.LeaseToken, err error) {
	evt := LeaseStateEvent{State: state, Token: token, Timestamp: m.clk.Now(), Err: err}
	for {
		select {
		case m.leaseEvents <- evt:
			return
		default:
		}
		// Buffer full: evict the oldest event and retry. The loop tolerates a
		// concurrent consumer draining between the failed send and the evict.
		select {
		case <-m.leaseEvents:
			m.leaseEventDrops.Add(1)
		default:
		}
	}
}

// Token returns the current lease token and whether the manager holds the lease.
func (m *Manager) Token() (persistence.LeaseToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token, m.hasLease
}

// Exclusive reports whether this session participates in lease-based failover.
// A non-exclusive session never acquires a lease, so it must not count toward
// the runtime's active/standby role classification (see roleUnlocked). The flag
// is set once at construction and never mutates, so no lock is needed.
func (m *Manager) Exclusive() bool { return m.exclusive }

// Close quiesces the source session and then releases a still-held lease, in
// that order — the same close-before-release discipline every other
// lease-surrendering path follows (step-down, activation failure,
// session-failure recovery).
//
// The order is the safety property. Releasing first publishes the partition to
// standbys while THIS node is still connected and subscribed for the whole
// duration of session.Close: the standby's next poll (a few seconds at most)
// seizes the lease and activates alongside an owner that has not stopped
// consuming. MQTT hides that behind client-ID takeover, but an exclusive AMQP
// 0-9-1 / 1.0 consumer really does end up double-consuming.
//
// The close is bounded and DETACHED (WithoutCancel), so an embedder calling
// Close AFTER cancelling the ctx it passed still tears the session down and
// still releases the lease — otherwise a cancelled ctx silently skips both and
// the partition stays owned for a full TTL before a standby can take over.
//
// A source Close that IGNORED its context and only the ceiling unblocked never
// reached its own teardown, so whether it stopped consuming is UNKNOWN and the
// release is skipped exactly as on the session-failure path: the lease stays
// held and expires by natural TTL, after process exit has forcibly torn the
// socket down at the OS level. A Close that RETURNS has stopped ingress even
// when it reports an error (a settlement that outran the drain budget, say), so
// that error is surfaced to the caller but does not block the hand-off.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	hadLease := m.hasLease
	token := m.token
	m.hasLease = false
	m.mu.Unlock()

	// The caller owns the teardown budget here: Runtime.Stop closes every managed
	// session sequentially under ONE deadline, so a per-manager budget of our own
	// would let n sessions overrun it n-fold and get the pod SIGKILLed mid-drain.
	// It also restores the meaning of the configured shutdown timeout for managed
	// sessions, which the step-down-derived release budget would otherwise
	// shorten. A caller with no deadline, or one already out of time, falls back
	// to the internal bounded-teardown budget.
	//
	// The remaining budget is computed via the injected clock (never time.Until),
	// matching Runtime.clampedStoreCloseGrace: m.clk is the real wall clock in
	// production — the same clock the caller's ctx deadline and the ceiling timer
	// use — and a fake only under test.
	ceiling := m.releaseTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := deadline.Sub(m.clk.Now()); remaining > 0 {
			ceiling = remaining
		}
	}
	closeErr, completed := m.closeSourceBounded(ctx, ceiling, "manager close")

	if hadLease && m.leaseStore != nil {
		if !completed {
			m.log(ctx, slog.LevelWarn,
				"source session close did not complete; keeping the lease until natural expiry "+
					"so a standby cannot overlap a still-subscribed session")
			return closeErr
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.releaseTimeout())
		if err := m.leaseStore.Release(releaseCtx, m.sessionID, token); err != nil {
			m.log(ctx, slog.LevelWarn, "lease release failed during close", "error", err)
		}
		cancel()
	}
	return closeErr
}
