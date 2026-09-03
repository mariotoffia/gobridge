package bridge

import (
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The coordinated cluster rollout's metric and deep-health publisher. It turns
// each applier observation, and each tick that produced none, into the series an
// operator alerts on and the snapshot they then read. Split from
// rollout_status.go, which owns RolloutStatus itself — the projection this
// produces and every composition root consumes.

// staleObservationTicks is how many poll intervals an observation may age before
// it is reported stale. Three tolerates one slow store call and one missed tick
// without crying wolf, while still going stale long before an operator could
// mistake a frozen snapshot for a live one.
const staleObservationTicks = 3

// rolloutObserver turns each applier observation into metrics and the
// deep-health snapshot. It is owned by the applier (single goroutine), except
// for the snapshot, which is read concurrently by health handlers.
type rolloutObserver struct {
	metrics ports.MetricsExporter
	// clk stamps and ages observations. Nil means the system clock (hand-wired
	// appliers in tests and benchmarks).
	clk clock.Clock
	// staleAfter is how old an observation may be before status() calls it
	// stale. Zero means staleObservationTicks x the default poll interval.
	staleAfter time.Duration

	mu   sync.RWMutex
	snap RolloutStatus
	// diverged is the last computed divergence level. It is republished on EVERY
	// tick rather than only on an observation: it is a level, not an event, and a
	// gauge that reports one datapoint and then goes silent cannot sustain an
	// alarm across its evaluation periods — missing data is filled as healthy.
	diverged bool
	// resolved records the last generation whose terminal outcome was counted,
	// so the resolution counter fires once per rollout rather than on every
	// poll of an already-decided row.
	resolved uint64
	// observedAt is when the rollout row was last successfully read; zero before
	// the first observation, where startedAt (when the drive began) is the
	// baseline instead — a drive that has NEVER managed a read is not fresh, it
	// just has nothing to be stale about yet. lastErr is the most recent
	// remote-call failure, kept so an operator reading a stale status learns WHY
	// it is stale rather than only that it is.
	startedAt  time.Time
	observedAt time.Time
	lastErr    string
	// artifactGen, terminalGen and terminalReason are this member's LOCAL safety
	// state, published alongside the shared row: which generation it has durably
	// recorded, and which generation's safe state it could not reach at all.
	artifactGen    uint64
	terminalGen    uint64
	terminalReason string
	// proposalRefusal is why this member's own barrier last refused to carry a
	// delta its config source delivered — the one reason a member goes silent
	// that nothing in the shared row can show. Cleared when a proposal succeeds
	// and when this member is seen to have voted.
	proposalRefusal string
}

// noteProposal records the outcome of this member's attempt to propose or join a
// rollout for a delta its own config source delivered. A refusal is the reason
// this member will not vote, so it is published beside the row it is silent on.
func (o *rolloutObserver) noteProposal(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err == nil {
		o.proposalRefusal = ""
		return
	}
	o.proposalRefusal = err.Error()
}

// newRolloutObserver builds the observer for one drive.
func newRolloutObserver(
	metrics ports.MetricsExporter, clk clock.Clock, pollInterval time.Duration, memberID string,
) *rolloutObserver {
	o := &rolloutObserver{
		metrics:    metrics,
		clk:        clk,
		staleAfter: staleObservationTicks * orDefault(pollInterval, defaultRolloutPollInterval),
	}
	// Identify the member from the start. Before the first successful read the
	// snapshot is otherwise indistinguishable from an observation of a cohort
	// with no rollout — and it is exactly then (a store that has never answered)
	// that an operator most needs to know WHICH member is reporting.
	o.snap.MemberID = memberID
	o.startedAt = o.now()
	return o
}

// now reads the observer's clock, defaulting to the system clock so a
// hand-wired observer (tests, benchmarks) needs no clock to work.
func (o *rolloutObserver) now() time.Time {
	if o.clk == nil {
		return clock.System.Now()
	}
	return o.clk.Now()
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
	o.observedAt = o.now()
	o.lastErr = ""
	if _, voted := r.Acks()[memberID]; voted {
		o.proposalRefusal = ""
	}
	if _, voted := r.Nacks()[memberID]; voted {
		o.proposalRefusal = ""
	}
	o.snap = RolloutStatus{
		MemberID:       memberID,
		Generation:     r.Generation(),
		State:          string(r.State()),
		ConfirmPending: r.State() == persistence.RolloutCommitted && !r.ConfirmDeadline().IsZero(),
		ConfigVersion:  r.ConfigVersion(),
		Epoch:          r.MembershipEpoch(),
		Acked:          sortedKeys(r.Acks()),
		Nacked:         sortedKeys(r.Nacks()),
		Converged:      sortedKeys(r.Converged()),
		Reason:         r.Reason(),
		Staged:         staged,
		Applied:        applied,
		NotVoting:      notVotingReason(r, memberID, staged, o.proposalRefusal),
	}
	o.diverged = rolloutDiverged(r, applied)
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

// rolloutDiverged reports whether a member that is not running the generation's
// content is genuinely out of step with the cohort. Only a generation the cohort
// has FINALLY decided can make it so, which rules out three states an alarm
// would otherwise fire on while the protocol works exactly as designed:
//
//   - proposed/staging decide nothing, and the whole cohort is on the previous
//     generation by design.
//   - a reverted (or aborted) rollout's correct outcome IS the previous
//     generation, so a member running it is right. One that could not get back
//     there reports that through the terminal signal instead.
//   - a PROVISIONAL commit — one carrying a confirm window (design §8.1) — has
//     not decided either. The window exists precisely to handle members that
//     cannot converge: it reverts the whole cohort. A member deliberately still
//     on N-1 during the window (it never staged the candidate, so the "reboot
//     reverts" rule keeps it on its safe config) is following the protocol, and
//     so is one whose expired window is waiting on a slow coordinator.
func rolloutDiverged(r persistence.Rollout, applied bool) bool {
	switch r.State() {
	case persistence.RolloutCommitted:
		return r.ConfirmDeadline().IsZero() && !applied
	case persistence.RolloutConfirmed:
		return !applied
	default:
		return false
	}
}

// boolGauge renders a boolean condition as the 0/1 a gauge carries.
func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
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

// status returns the last observation, aged against the clock. Freshness is
// computed at READ time, not at observation time, so a drive that has stopped
// observing entirely — the store black-holed, the goroutine gone — reports a
// growing age rather than a snapshot frozen at the moment it last worked.
func (o *rolloutObserver) status() RolloutStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := o.snap
	out.LastError = o.lastErr
	out.ArtifactGeneration = o.artifactGen
	out.TerminalGeneration = o.terminalGen
	out.TerminalReason = o.terminalReason
	out.ObservedAt = o.observedAt
	if since := o.freshnessBaseline(); !since.IsZero() {
		out.ObservationAge = o.now().Sub(since)
		out.Stale = out.ObservationAge > o.staleBudget()
	}
	return out
}

// freshnessBaseline is the instant the observation age is measured from: the
// last successful read, or — before there has been one — when the drive started.
// Callers hold at least a read lock.
func (o *rolloutObserver) freshnessBaseline() time.Time {
	if !o.observedAt.IsZero() {
		return o.observedAt
	}
	return o.startedAt
}

// staleBudget is how old an observation may be before it is stale.
func (o *rolloutObserver) staleBudget() time.Duration {
	return orDefault(o.staleAfter, staleObservationTicks*defaultRolloutPollInterval)
}

// observeAbsent records a successful read that found NO rollout row. It is a
// genuine observation of the shared row — "the cohort has no rollout in flight"
// is an answer, not a missing one — so it keeps this member's freshness signal
// alive during the long stretches between rollouts.
func (o *rolloutObserver) observeAbsent() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.observedAt = o.now()
	o.lastErr = ""
	// Clear what the ROW said. Keeping the last row's fields under a fresh
	// timestamp would report a rollout that no longer exists as currently
	// observed — "committed, not applied on this member", stale=false — which
	// latches a divergence that nothing can ever resolve. The member's own local
	// safety state (artifact, terminal) is not row-derived and survives.
	o.snap = RolloutStatus{MemberID: o.snap.MemberID}
	o.diverged = false
	o.mu.Unlock()
	if o.metrics != nil {
		o.metrics.Gauge(shared.MetricClusterRolloutDiverged, 0)
	}
}

// observeCall meters one remote call the barrier made. It is nil-safe because
// the proposer path runs before the drive exists.
func (o *rolloutObserver) observeCall(class, outcome string, err error) {
	if o == nil {
		return
	}
	if err != nil {
		o.mu.Lock()
		o.lastErr = err.Error()
		o.mu.Unlock()
	}
	if o.metrics != nil {
		o.metrics.Counter(shared.MetricClusterRolloutStoreCalls, 1,
			shared.Tag{Key: shared.TagKeyOperation, Value: class},
			shared.Tag{Key: shared.TagKeyOutcome, Value: outcome})
	}
}

// observeRetry publishes how many consecutive attempts a local safety operation
// has taken without completing; zero once it completes.
func (o *rolloutObserver) observeRetry(class string, attempts int) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.Gauge(shared.MetricClusterRolloutRetries, float64(attempts),
		shared.Tag{Key: shared.TagKeyOperation, Value: class})
}

// observeArtifact records the generation whose durable committed artifact this
// member has VERIFIED as written.
func (o *rolloutObserver) observeArtifact(gen uint64) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.artifactGen = gen
	o.mu.Unlock()
}

// observeTerminal records a generation whose safe state this member could not
// reach. It is a latch: the member cannot repair itself and needs replacing.
func (o *rolloutObserver) observeTerminal(gen uint64, reason string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.terminalGen, o.terminalReason = gen, reason
	o.mu.Unlock()
	if o.metrics != nil {
		o.metrics.Gauge(shared.MetricClusterRolloutTerminal, float64(gen))
	}
}

// publishLevels emits the three gauges that describe a STATE rather than an
// event: how old this member's observation is, whether it is diverged from the
// cohort's decision, and which generation's safe state it cannot reach.
//
// The drive calls it every tick, and that is the whole point. All three are
// levels, and a level published once and then left silent cannot sustain an
// alarm: an alarm spanning several evaluation periods fills the missing ones,
// and "missing data is not breaching" is the only sane default for a series a
// deployment without the barrier never emits. A terminal member that published
// one datapoint and went quiet would therefore never page — on the one condition
// whose documented remedy is operator action. Republishing also keeps the
// divergence level alive through ticks whose store read failed, where the
// observation age says how much to trust it.
func (o *rolloutObserver) publishLevels() {
	if o == nil || o.metrics == nil {
		return
	}
	o.mu.RLock()
	since := o.freshnessBaseline()
	diverged, terminalGen := o.diverged, o.terminalGen
	o.mu.RUnlock()

	o.metrics.Gauge(shared.MetricClusterRolloutDiverged, boolGauge(diverged))
	o.metrics.Gauge(shared.MetricClusterRolloutTerminal, float64(terminalGen))
	if since.IsZero() {
		return
	}
	o.metrics.Gauge(shared.MetricClusterRolloutObservationAge, o.now().Sub(since).Seconds())
}

// notVotingReason says why memberID has not voted on r, in the words an operator
// needs to act, and "" once it has voted or has nothing standing in its way.
//
// The three causes are three different jobs. A member outside the frozen epoch
// is a roster or an identity that does not match the deployment. A member whose
// barrier refused the delta is a change the cohort cannot agree on and needs
// re-proposing, usually because this member and the proposer do not read the same
// document. A member that never staged the candidate is a lagging config source,
// which the deadline already bounds and which usually needs nothing at all.
func notVotingReason(r persistence.Rollout, memberID string, staged bool, refusal string) string {
	if r.IsTerminal() {
		return ""
	}
	if _, voted := r.Acks()[memberID]; voted {
		return ""
	}
	if _, voted := r.Nacks()[memberID]; voted {
		return ""
	}
	if !slices.Contains(r.MembershipEpoch(), memberID) {
		return "this member is not in the rollout's frozen membership epoch, so its vote could never " +
			"be counted; reconcile bridge.cluster.members against this member's identity"
	}
	if refusal != "" {
		return "this member's barrier refused to carry the delta its own config source delivered: " + refusal
	}
	if !staged {
		return "this member's own config source has not delivered the candidate config yet, so it has " +
			"nothing to vote on"
	}
	return ""
}
