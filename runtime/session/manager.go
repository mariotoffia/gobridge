package session

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// LeaseState describes the lifecycle state of a lease transition.
type LeaseState int

const (
	LeaseStateNone LeaseState = iota
	LeaseStateAcquired
	LeaseStateRenewed
	LeaseStateLost
	LeaseStateSteppedDown
	LeaseStateReleased
	// LeaseStateReconcileFailed signals a reconcile-on-reconnect failure while
	// the lease was still held and renewing. It is emitted INSTEAD OF
	// LeaseStateLost so a subscription blip is never mis-observed as a lease
	// loss/transfer; the session is surfaced to superviseSession for isolated
	// restart. Appended last to keep the existing iota values stable.
	LeaseStateReconcileFailed
)

// LeaseStateEvent is emitted whenever the lease state changes.
type LeaseStateEvent struct {
	State     LeaseState
	Token     persistence.LeaseToken
	Timestamp time.Time
	Err       error
}

// Manager manages the lifecycle of a single session, including
// lease acquisition, renewal, three-phase step-down, and reconciliation.
type Manager struct {
	sessionID         string
	session           ports.Session
	leaseStore        ports.LeaseStore
	ownerID           string
	exclusive         bool
	connectAfterLease bool
	plan              connectivity.SessionPlan
	leaseTTL          time.Duration
	renewInterval     time.Duration
	renewJitter       time.Duration
	acquirePoll       time.Duration
	renewCallTimeout  time.Duration
	maxRenewFails     int
	stepDownGrace     time.Duration
	activationTimeout time.Duration
	// brokerHealthStepDown, when > 0, bounds how long this active owner may stay
	// non-converged on its broker path before it steps down so a standby can take
	// over a node-local broker outage (CLUSTER-2). Zero disables the check.
	brokerHealthStepDown time.Duration
	endpoints            map[string]string
	drainIdle            func() bool
	metrics              ports.MetricsExporter
	audit                ports.AuditLogger
	logger               *slog.Logger
	clk                  clock.Clock

	mu            sync.Mutex
	token         persistence.LeaseToken
	hasLease      bool
	connectedOnce atomic.Bool
	// leaseDeadline is the local, fail-closed expiry of the lease we hold: the
	// pre-call timestamp of the last SUCCESSFUL Acquire/Renew plus LeaseTTL.
	// Once passed, the renew loop steps down UNCONDITIONALLY — even if a
	// write-fails/read-succeeds partition keeps Current naming us as owner — so
	// this owner can never consume past the instant a standby can seize the
	// expired lease (split-brain renew-fail/read-succeed fix). Guarded by mu.
	leaseDeadline time.Time
	// notConvergedSince is the time this owner's broker path first became
	// non-converged (disconnected / not re-subscribed) AFTER having been converged.
	// Zero means converged (healthy) or never-yet-converged. The renew loop steps
	// down once now-notConvergedSince exceeds brokerHealthStepDown (CLUSTER-2).
	// Guarded by mu.
	//
	// It is cleared ONLY by markConverged (a SessionConnected + successful reconcile
	// event), NOT on lease renew/acquire — renewals deliberately keep succeeding
	// during a broker outage, so clearing on renew would defeat the whole feature.
	// This relies on a fresh Manager per lease term: every shipped exclusive
	// transport is single-use, so a lease loss/step-down escalates to a pod restart
	// (new Manager, zero timestamp) rather than an in-loop re-acquire. A future
	// MULTI-use exclusive transport that re-acquires without a new process MUST
	// reset notConvergedSince at the start of the new term (before the first renew
	// tick) or it could step down spuriously on a stale timestamp.
	notConvergedSince time.Time

	leaseEvents     chan LeaseStateEvent
	leaseEventDrops atomic.Uint64
}
