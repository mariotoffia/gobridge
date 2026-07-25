package bridge

import (
	"slices"
	"sync"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout observability (design §9).
//
// A rollout is the one config change whose outcome is not local: this member can
// be perfectly healthy while the cohort's barrier is stuck behind a peer that
// never acked. Neither the reload counter nor the degraded gauge would show
// that, so the barrier publishes its own state — as metrics for alerting and as
// a deep-health section for the operator who then has to diagnose it.

// RolloutStatus is the deep-health projection of the coordinated cluster rollout
// barrier, as this member last observed it. It is a read-only snapshot; an empty
// Acked slice with a non-empty Epoch is the normal shape of a rollout nobody has
// voted on yet.
type RolloutStatus struct {
	// MemberID is this node's identity within the cohort.
	MemberID string
	// Generation is the observed rollout generation; 0 when none exists.
	Generation uint64
	// State is the observed rollout state ("proposed" | "staging" | "committed"
	// | "aborted" | "confirmed" | "reverted"), empty when no rollout has ever been
	// proposed. "committed" with a non-empty Converged/ConfirmPending is a confirm
	// window in progress (design §8.1).
	State string
	// ConfigVersion is the config version the rollout carries.
	ConfigVersion int
	// Epoch is the frozen membership epoch, sorted.
	Epoch []string
	// Acked and Nacked are the members that have voted, sorted. Epoch minus
	// Acked minus Nacked is exactly who the cohort is waiting for.
	Acked  []string
	Nacked []string
	// Converged lists the members that recorded post-swap convergence during an
	// active confirm window (design §8.1), sorted. Epoch minus Converged is who the
	// confirm barrier is waiting for before it can Confirm; the ones still missing
	// when the window expires are why the cohort reverts.
	Converged []string
	// Reason carries the abort reason (or nack aggregation) when present.
	Reason string
	// Staged reports whether THIS member has the candidate config its own
	// config source must deliver before it can vote. A false here on a
	// long-staging rollout identifies this member as the one holding it up.
	Staged bool
	// Applied reports whether this member is actually RUNNING the generation.
	// It is meaningful only once State is "committed", and it is the signal that
	// distinguishes a healthy cohort from a split one: the rollout row says
	// committed on every member, but a member whose local swap failed is still
	// on the previous generation. Alert on committed AND NOT applied.
	Applied bool
}

// rolloutObserver turns each applier observation into metrics and the
// deep-health snapshot. It is owned by the applier (single goroutine), except
// for the snapshot, which is read concurrently by health handlers.
type rolloutObserver struct {
	metrics ports.MetricsExporter

	mu   sync.RWMutex
	snap RolloutStatus
	// resolved records the last generation whose terminal outcome was counted,
	// so the resolution counter fires once per rollout rather than on every
	// poll of an already-decided row.
	resolved uint64
}

// rolloutStates returns every state the gauge reports, so exactly one series
// reads 1 and the rest read 0 — a gauge that only ever set the CURRENT state
// would leave the previous state latched at 1 forever in any pull-based
// exporter.
func rolloutStates() []persistence.RolloutState {
	return []persistence.RolloutState{
		persistence.RolloutProposed,
		persistence.RolloutStaging,
		persistence.RolloutCommitted,
		persistence.RolloutAborted,
		persistence.RolloutConfirmed,
		persistence.RolloutReverted,
	}
}

// observe publishes one observation. staged reports whether this member holds
// the candidate config for the observed digest; applied reports whether it is
// actually running that generation's content.
func (o *rolloutObserver) observe(r persistence.Rollout, memberID string, staged, applied bool) {
	o.mu.Lock()
	o.snap = RolloutStatus{
		MemberID:      memberID,
		Generation:    r.Generation(),
		State:         string(r.State()),
		ConfigVersion: r.ConfigVersion(),
		Epoch:         r.MembershipEpoch(),
		Acked:         sortedKeys(r.Acks()),
		Nacked:        sortedKeys(r.Nacks()),
		Converged:     sortedKeys(r.Converged()),
		Reason:        r.Reason(),
		Staged:        staged,
		Applied:       applied,
	}
	// Count a terminal outcome exactly once per generation. Done under the same
	// lock as the snapshot so the counter and the snapshot cannot disagree about
	// which generation was last resolved. Window-aware: a provisional (windowed)
	// commit is NOT yet resolved — it counts only at Confirmed/Reverted.
	countResolution := r.IsTerminal() && o.resolved != r.Generation()
	if countResolution {
		o.resolved = r.Generation()
	}
	o.mu.Unlock()

	if o.metrics == nil {
		return
	}
	for _, st := range rolloutStates() {
		value := 0.0
		if st == r.State() {
			value = 1
		}
		o.metrics.Gauge(shared.MetricClusterRolloutState, value,
			shared.Tag{Key: shared.TagKeyState, Value: string(st)})
	}
	o.metrics.Gauge(shared.MetricClusterRolloutAcks, float64(len(r.Acks())))
	o.metrics.Gauge(shared.MetricClusterRolloutEpoch, float64(len(r.MembershipEpoch())))
	if countResolution {
		o.metrics.Counter(shared.MetricClusterRolloutResolved, 1,
			shared.Tag{Key: shared.TagKeyOutcome, Value: rolloutOutcome(r.State())})
	}
}

// rolloutOutcome maps a terminal rollout state to the resolution counter's
// outcome tag. Committed is the base-protocol success; Confirmed/Reverted are the
// confirm-window (design §8.1) terminal outcomes; anything else is an abort.
func rolloutOutcome(state persistence.RolloutState) string {
	switch state {
	case persistence.RolloutCommitted:
		return "committed"
	case persistence.RolloutConfirmed:
		return "confirmed"
	case persistence.RolloutReverted:
		return "reverted"
	default:
		return "aborted"
	}
}

// status returns the last observation.
func (o *rolloutObserver) status() RolloutStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.snap
}

// RolloutStatus returns this member's last observation of the coordinated
// cluster rollout barrier, and false when the deployment does not run one. It is
// safe for concurrent use and never blocks on a store call — health probes read
// the last observation, they do not trigger a new one.
func (s *Supervisor) RolloutStatus() (RolloutStatus, bool) {
	if s.rolloutDriver == nil {
		return RolloutStatus{}, false
	}
	return s.rolloutDriver.Status()
}

// sortedKeys renders a vote map as the sorted member list health output and
// abort reasons need (map iteration order is undefined).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
