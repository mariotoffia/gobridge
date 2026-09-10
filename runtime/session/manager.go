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
	// serving records that this term has reached the point where it is DUE to be
	// serving — a completed post-acquire activation, or a SessionConnected event
	// followed by a successful reconcile. It gates the broker-health outage
	// clock, because a broker path that is down before then is activation, not an
	// OUTAGE, and is bounded separately by the activation timeout.
	//
	// It is deliberately NOT connectedOnce, which counts reconnects: a transport
	// event channel drops its oldest entry under a storm, so an owner that
	// genuinely converged may never see its own SessionConnected event, and
	// gating the outage clock on that event left broker-path failover silently
	// disarmed for the rest of the term.
	serving atomic.Bool
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
	// It is cleared by markConverged (a completed post-acquire activation against a
	// connected source, or a SessionConnected event followed by a successful
	// reconcile) and by beginBrokerPathTerm at the start of each term — NOT on
	// lease renew/acquire, because renewals deliberately keep succeeding during a
	// broker outage and clearing on renew would defeat the whole feature.
	//
	// The per-term reset matters because a term can end while the clock is armed
	// for a reason OTHER than this threshold — a definitive fencing loss during a
	// broker blip, say — and the caller then re-acquires in place. Without the
	// reset that next term's first renew tick, which fires while it is still
	// connecting, would find itself due on the previous term's timestamp and step
	// down at once. (A term ended BY this threshold does not re-acquire at all: it
	// is terminal for the process, see errBrokerPathStepDown.)
	notConvergedSince time.Time

	leaseEvents     chan LeaseStateEvent
	leaseEventDrops atomic.Uint64
}
