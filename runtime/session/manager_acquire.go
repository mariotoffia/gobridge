package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Standby acquisition and the per-event bounds a term uses once it owns the
// lease: the acquire/poll loop with its outage escalation, the reconnect
// reconcile ceiling, and the authoritative ownership read.

func (m *Manager) acquireLeaseWithRetry(ctx context.Context) (persistence.LeaseToken, error) {
	leaseTag := shared.Tag{Key: shared.TagKeyLeaseID, Value: m.sessionID}
	// Rate-limited escalation for a lease-store OUTAGE. Normal
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
		// takeover is not delayed by up to a full renew interval.
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
// alongside the new owner — two live consumers for unbounded time.
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
// TRANSIENT renew failures actually cost us the lease. It returns
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
