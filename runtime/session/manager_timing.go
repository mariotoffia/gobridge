package session

import (
	"context"
	"log/slog"
	rand "math/rand/v2"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// Per-term timing primitives: fencing-token bookkeeping, the local lease
// deadline that fails closed at expiry, the jittered renew and acquire delays,
// and the shared log/audit emitters.

func (m *Manager) setToken(token persistence.LeaseToken) {
	m.mu.Lock()
	m.token = token
	m.hasLease = true
	m.mu.Unlock()
}

// recordLeaseDeadline records the local, fail-closed lease expiry from the
// pre-call timestamp of a SUCCESSFUL Acquire/Renew (start), which the caller
// captured BEFORE issuing the store write. Using the pre-call time (rather than
// the post-call time) keeps the local deadline at or before the store's
// authoritative ExpiresAt: the store computes ExpiresAt = store-write-time + TTL
// and the write happens at or after start, so start+TTL <= ExpiresAt. That
// guarantees a forced step-down at this deadline fires at or before the instant
// a standby can seize the expired lease — the fail-closed posture the
// split-brain (renew-fail/read-succeed) fix requires.
func (m *Manager) recordLeaseDeadline(start time.Time) {
	m.mu.Lock()
	m.leaseDeadline = start.Add(m.leaseTTL)
	m.mu.Unlock()
}

// leaseDeadlinePassed reports whether the injected clock has reached the local
// lease deadline recorded by the last successful Acquire/Renew. When true the
// renew loop MUST step down unconditionally (fail-closed), regardless of any
// authoritative Current read that still names us — the write-fails/read-succeeds
// partition this closes keeps Current optimistic past our real expiry.
func (m *Manager) leaseDeadlinePassed() bool {
	m.mu.Lock()
	deadline := m.leaseDeadline
	m.mu.Unlock()
	return !deadline.IsZero() && !m.clk.Now().Before(deadline)
}

// jitter returns a random offset in [-renewJitter/2, +renewJitter/2).
func (m *Manager) jitter() time.Duration {
	if m.renewJitter <= 0 {
		return 0
	}
	half := m.renewJitter / 2
	return time.Duration(rand.Int64N(int64(m.renewJitter))) - half
}

// clampedInterval returns renewInterval + jitter, floored at 1ms to
// prevent near-zero or negative timer durations when jitter exceeds
// the renewal interval.
func (m *Manager) clampedInterval() time.Duration {
	return max(m.renewInterval+m.jitter(), time.Millisecond)
}

// nextRenewDelay is clampedInterval, additionally clamped so the renew timer
// NEVER fires after the local lease deadline: scheduling a renew beyond expiry
// is exactly the split-brain window this closes (a write-failing owner would
// otherwise keep its next renewal — and its consumption — past the point a
// standby seizes the lease). When the deadline is already at or in the past the
// timer is floored to fire almost immediately so the renew loop's
// deadline-passed check runs and forces a fail-closed step-down. Floored at 1ms
// (like clampedInterval) to keep the fake-clock timer well-formed.
func (m *Manager) nextRenewDelay() time.Duration {
	d := m.clampedInterval()
	m.mu.Lock()
	deadline := m.leaseDeadline
	m.mu.Unlock()
	if !deadline.IsZero() {
		if remaining := deadline.Sub(m.clk.Now()); remaining < d {
			d = remaining
		}
	}
	return max(d, time.Millisecond)
}

// acquirePollDelay returns the standby lease-acquisition poll interval with a
// small de-synchronising jitter. Standbys poll on a DEDICATED cadence
// (m.acquirePoll), decoupled from the (typically much larger) owner renew
// interval, so a takeover is not delayed by up to a full renew interval
// The ±25% jitter spreads competing standbys so they do not
// stampede the lease store on expiry. Floored at 1ms.
func (m *Manager) acquirePollDelay() time.Duration {
	base := m.acquirePoll
	if base <= 0 {
		base = m.renewInterval
	}
	if base <= 0 {
		return time.Millisecond
	}
	spread := base / 2 // full width of the jitter window (±25% of base)
	var j time.Duration
	if spread > 0 {
		j = time.Duration(rand.Int64N(int64(spread))) - spread/2
	}
	return max(base+j, time.Millisecond)
}

func (m *Manager) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if m.logger == nil || !m.logger.Enabled(ctx, level) {
		return
	}
	allArgs := append([]any{"session_id", m.sessionID}, args...)
	m.logger.Log(ctx, level, msg, allArgs...)
}

func (m *Manager) emitLeaseAudit(ctx context.Context, action, outcome string, token persistence.LeaseToken, err error) {
	detail := map[string]any{
		"owner_id": m.ownerID,
		"version":  token.Version,
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	m.audit.Log(ctx, ports.AuditEvent{
		Timestamp:  m.clk.Now().UTC(),
		Action:     action,
		Actor:      m.ownerID,
		Resource:   "lease",
		ResourceID: m.sessionID,
		Outcome:    outcome,
		Detail:     detail,
	})
}
