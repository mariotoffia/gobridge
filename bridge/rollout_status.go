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
	// ObservationAge is how long ago this member last read the rollout row, and
	// Stale reports that the age has outrun the barrier's poll cadence. Every
	// other field is a projection of that observation, so an operator must read
	// these two first: a stale status describes the cohort as it WAS.
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
}

// newRolloutObserver builds the observer for one drive.
func newRolloutObserver(metrics ports.MetricsExporter, clk clock.Clock, pollInterval time.Duration) *rolloutObserver {
	o := &rolloutObserver{
		metrics:    metrics,
		clk:        clk,
		staleAfter: staleObservationTicks * orDefault(pollInterval, defaultRolloutPollInterval),
	}
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
	o.mu.Unlock()
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

// publishFreshness emits the observation-age gauge. The drive calls it every
// tick — including ticks whose store read failed, which are exactly the ticks
// that make the age worth reporting.
func (o *rolloutObserver) publishFreshness() {
	if o == nil || o.metrics == nil {
		return
	}
	o.mu.RLock()
	since := o.freshnessBaseline()
	o.mu.RUnlock()
	if since.IsZero() {
		return
	}
	o.metrics.Gauge(shared.MetricClusterRolloutObservationAge, o.now().Sub(since).Seconds())
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
