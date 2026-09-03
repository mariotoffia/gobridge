package bridge

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
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
	// proposed.
	State string
	// ConfirmPending reports that the observed "committed" state is PROVISIONAL:
	// a confirm window (design §8.1) is open, so the cohort has not decided yet
	// and will revert if the window expires before every member converges. It is
	// the difference between "this member is behind the cohort" and "the cohort
	// is still making up its mind", which is the difference between paging and
	// waiting.
	ConfirmPending bool
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
	// NotVoting says WHY this member has not voted on the observed rollout, and
	// is empty once it has (or when it has nothing standing in its way).
	//
	// Staged alone reports only that the candidate is missing, which is the
	// benign case — a lagging watcher the deadline already bounds. The case an
	// operator cannot diagnose without this is the member whose config source DID
	// deliver the change and whose barrier then refused to join the rollout
	// carrying it. Without a reason here, the whole cohort reports "waiting for
	// acks" and the only account of the refusal is a line in one member's log.
	NotVoting string
	// Applied reports whether this member is actually RUNNING the generation's
	// config — answered by comparing its running config against the digest the
	// cohort agreed on, so it holds after a restart, after a catch-up from the
	// durable artifact, and for a member that never staged the candidate.
	//
	// It is the signal that distinguishes a healthy cohort from a split one: the
	// rollout row reads committed on every member, but a member whose local swap
	// failed is still on the previous generation. Alert on a FINAL commit (or a
	// confirm) that is not applied — see ConfirmPending.
	Applied bool
	// ObservedAt is when this member last successfully read the rollout row, and
	// zero before it has ever managed one. ObservationAge is the same fact
	// relative to the reader's own clock, and Stale reports that the age has
	// outrun the barrier's poll cadence. Every other field is a projection of that
	// observation, so an operator must read these first: a stale status describes
	// the cohort as it WAS. The absolute instant is what makes two MEMBERS'
	// snapshots comparable — each one's age is measured at its own read time, so
	// ages alone cannot say whose view is older.
	ObservedAt     time.Time
	ObservationAge time.Duration
	Stale          bool
	// LastError is the most recent remote-call failure, so a stale status says
	// why it is stale.
	LastError string
	// ArtifactGeneration is the generation whose durable last-committed config
	// artifact this member has VERIFIED as written — what it would boot on. It
	// lags Generation while a write is being retried.
	ArtifactGeneration uint64
	// TerminalGeneration is a generation whose safe state this member could not
	// reach (a committed config it could not durably record, or a provisional one
	// it could not revert), and zero when there is none. Non-zero means this
	// member cannot repair itself and must be replaced; TerminalReason says which
	// of the two it was.
	TerminalGeneration uint64
	TerminalReason     string
}

// DegradedState reports whether the coordinated rollout makes this member's
// live-reconfiguration health degraded, and why. Every composition root that
// surfaces a rollout in deep health calls it, so the shipped AWS root and the
// reference binary cannot answer the same question differently.
//
// The barrier is atomic BEFORE the commit and per-member AFTER it (ADR 0013), so
// a member that has not yet applied a decided generation is not a protocol
// violation — it is the convergence window, and the window is only safe because
// it is VISIBLE. These three rules are what make it visible on the field every
// deployment's health check already watches:
//
//   - decided but not applied: this member runs an older generation than its
//     peers. Left un-degraded it looks identical to a converged member, because
//     every other signal describes the shared row, which reads the same on both.
//     A PROVISIONAL commit is excluded for the same reason the divergence gauge
//     excludes it: the confirm window itself handles a member that cannot
//     converge, by reverting the cohort.
//   - stale observation: the block is a snapshot of a row this member can no
//     longer read, so "committed everywhere, all acked" may be minutes out of
//     date. A stale observer that reports healthy is worse than one reporting
//     nothing, because it answers the operator's question wrongly.
//   - terminal: this member cannot reach the generation's safe state on its own
//     and needs an operator; the reason says which action.
func (r RolloutStatus) DegradedState() (bool, string) {
	var reasons []string
	if r.divergedFromCohort() {
		reasons = append(reasons, fmt.Sprintf(
			"coordinated cluster rollout generation %d (config version %d) is %s for the cohort but is "+
				"NOT applied on this member, which runs an older config generation than its peers",
			r.Generation, r.ConfigVersion, r.State))
	}
	if r.Stale {
		reason := fmt.Sprintf(
			"the coordinated cluster rollout observation is stale (%s old); this member cannot read the "+
				"rollout row, so the cohort state reported here describes the cohort as it WAS",
			r.ObservationAge.Round(time.Second))
		if r.LastError != "" {
			reason += ": " + r.LastError
		}
		reasons = append(reasons, reason)
	}
	if r.TerminalGeneration != 0 {
		reasons = append(reasons, r.TerminalReason)
	}
	if len(reasons) == 0 {
		return false, ""
	}
	return true, strings.Join(reasons, "; ")
}

// divergedFromCohort is DegradedState's half of the divergence question, kept in
// step with the rolloutDiverged gauge: only a FINAL decision counts, so the two
// signals never disagree about the same observation.
func (r RolloutStatus) divergedFromCohort() bool {
	if r.Applied || r.ConfirmPending {
		return false
	}
	return r.State == string(persistence.RolloutCommitted) ||
		r.State == string(persistence.RolloutConfirmed)
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
