package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The convergence contract of the coordinated cluster rollout (ADR 0013).
//
// The barrier is atomic BEFORE the commit and per-member AFTER it: the cohort
// decides once, then each member converges on its own. That contract is only
// honest if the window between the two is bounded and visible — a member that
// quietly stays on the previous generation forever is the mixed-version cohort
// the barrier exists to prevent, wearing a healthy status.
//
// So a member that cannot apply a decided generation must (a) publish the
// divergence as a fleet-alertable signal, (b) keep repairing itself under a
// bounded backoff so a transient cause converges without an operator, and (c)
// declare itself terminal once the bound is reached, because a member that
// cannot reach the cohort's decision needs replacing.

// TestRolloutObserver_PublishesDivergenceForADecidedGenerationItIsNotRunning
// pins the fleet signal. Every other rollout series describes the SHARED row,
// which reads "committed" identically on a converged member and on one whose
// swap failed — so without a per-member divergence gauge a split cohort has no
// metric at all, only a deep-health field nobody polls across the fleet.
func TestRolloutObserver_PublishesDivergenceForADecidedGenerationItIsNotRunning(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := soloCohortConfig(3)
	seedRollout(t, store, cfg, persistence.RolloutCommitted)
	r, err := store.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutCommitted, r.State())

	for _, tc := range []struct {
		name    string
		applied bool
		want    float64
	}{
		{name: "a member running the committed generation is converged", applied: true, want: 0},
		{name: "a member that has not applied it is diverged", applied: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &ports.RecordingExporter{}
			obs := &rolloutObserver{metrics: rec}

			obs.observe(r, "node-a", true, tc.applied)
			obs.publishLevels() // the drive publishes levels once per tick

			got, found := gaugeValue(rec, shared.MetricClusterRolloutDiverged, "", "")
			require.True(t, found, "the divergence gauge must be emitted on every observation, "+
				"including the healthy one — a gauge that only appears when broken never returns to 0")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRolloutObserver_ReportsNoDivergenceBeforeTheCohortDecides proves the gauge
// answers "is this member running what the cohort DECIDED", not "has it applied
// the candidate". A proposed or staging rollout has decided nothing, so a member
// still on the previous generation is exactly where the protocol wants it —
// alarming there would page on every healthy rollout.
func TestRolloutObserver_ReportsNoDivergenceBeforeTheCohortDecides(t *testing.T) {
	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 3,
		Members: []string{"node-a", "node-b"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	rec := &ports.RecordingExporter{}
	obs := &rolloutObserver{metrics: rec}

	obs.observe(r, "node-a", true, false)
	obs.publishLevels()

	got, found := gaugeValue(rec, shared.MetricClusterRolloutDiverged, "", "")
	require.True(t, found)
	assert.Zero(t, got, "an undecided rollout cannot make a member diverged")
}

// TestRolloutObserver_StatusCarriesTheObservationInstant pins the absolute
// timestamp beside the relative age. The age is measured against the reader's
// own clock at read time, so two members' ages cannot be compared; the instant
// can. An operator diagnosing a split cohort needs to know whether member B's
// snapshot is older than member A's, which is a question only a timestamp
// answers.
func TestRolloutObserver_StatusCarriesTheObservationInstant(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	obs := newRolloutObserver(nil, fake, time.Second, "node-a")

	before := obs.status()
	assert.True(t, before.ObservedAt.IsZero(),
		"a drive that has never read the row has no observation instant to report")

	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 3,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	fake.Advance(30 * time.Second)
	obs.observe(r, "node-a", true, false)

	got := obs.status()
	assert.Equal(t, fake.Now(), got.ObservedAt)
	assert.Zero(t, got.ObservationAge, "the age is measured from the instant just recorded")

	fake.Advance(10 * time.Second)
	aged := obs.status()
	assert.Equal(t, got.ObservedAt, aged.ObservedAt, "the instant is when the row was READ, not when it was reported")
	assert.Equal(t, 10*time.Second, aged.ObservationAge)
}

// TestRolloutApplier_LogsTheRosterWhenItAbstainsOutsideTheEpoch pins the
// diagnosis for a rollout that deadline-aborts with no local trace. A member
// outside the frozen epoch is silent by design — correctly, since the aggregate
// would reject its vote — but silence on the one node an operator SSHes into
// leaves the abort with no local cause at all. The log names this member and the
// roster it is missing from, which is the whole diagnosis.
func TestRolloutApplier_LogsTheRosterWhenItAbstainsOutsideTheEpoch(t *testing.T) {
	var logs bytes.Buffer
	store := memoryrollout.NewStore()
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)
	host.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-b", ConfigDigest: digest, ConfigVersion: 7,
		Members: []string{"node-b", "node-c"}, TTL: time.Hour,
	})
	require.NoError(t, err)

	applier := &rolloutApplier{host: host, barrier: &rolloutBarrier{}, store: store, memberID: "node-a"}
	require.NoError(t, applier.vote(context.Background(), r))

	out := logs.String()
	assert.Contains(t, out, "node-a", "the log must name the member that abstained")
	assert.Contains(t, out, "node-b", "the log must name the roster it is missing from")
	assert.Contains(t, out, "node-c")
	assert.NotContains(t, r.Acks(), "node-a", "abstaining must not cast a vote")
}

// TestRolloutApplier_ReportsAppliedAfterConvergingFromTheDurableArtifact is the
// false-alarm guard on the divergence signal.
//
// A member that was down when its config source delivered the candidate never
// STAGES it, so it can never compare the running config against a staged
// candidate. It converges instead through the durable committed artifact, which
// it decodes and applies without staging anything. Answering "applied" from the
// staged candidate alone reports that correctly-converged member as diverged for
// as long as the committed row stays current — which pages an operator, and
// worse, teaches them the fleet convergence alarm cries wolf.
func TestRolloutApplier_ReportsAppliedAfterConvergingFromTheDurableArtifact(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	rec := &ports.RecordingExporter{}
	f.applier.obs = &rolloutObserver{metrics: rec}
	require.Equal(t, 0, f.sup.Config().Version, "precondition: running the initial config")

	// A peer proposed and committed generation 1 and wrote the durable artifact.
	// This member never staged the candidate: its own config source never
	// delivered it.
	committed := liveSafeCandidate(99)
	digest, ok := configCanonicalBytesDigest(committed)
	require.True(t, ok)
	r, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-b", ConfigDigest: digest, ConfigVersion: 99,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	commitAs(t, f.store, r.Generation(), "node-a")
	seedCommittedArtifact(t, f.store, codec, committed, r.Generation())

	// First observation: not yet converged, and honestly reported as diverged.
	require.NoError(t, f.applier.step(context.Background()))
	require.Equal(t, 99, f.sup.Config().Version, "the member converges from the artifact")

	// Second observation: the committed row is still current, and the member is
	// now running it.
	require.NoError(t, f.applier.step(context.Background()))
	f.applier.obs.publishLevels()

	status := f.applier.obs.status()
	assert.Equal(t, string(persistence.RolloutCommitted), status.State)
	assert.True(t, status.Applied,
		"a member running the committed generation must report applied, staged candidate or not")
	diverged, found := gaugeValue(rec, shared.MetricClusterRolloutDiverged, "", "")
	require.True(t, found)
	assert.Zero(t, diverged, "a converged member must not hold the fleet divergence alarm open")
}

// TestRolloutApplier_ReportsNotAppliedAfterALocalRevertWithoutACandidate closes
// the one hole in answering "applied" from the local applied-generation mark.
//
// The mark advances on a verified swap and never rewinds — but the
// confirm-window deadman deliberately puts this member BACK on N-1 after it
// provisionally applied N. If its config source has since replaced the staged
// candidate (an operator rolling the source back), there is no content to
// compare against and the mark alone would report the member as running a
// generation it deliberately gave up.
func TestRolloutApplier_ReportsNotAppliedAfterALocalRevertWithoutACandidate(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	candidate := liveSafeCandidate(99)
	r := f.seedForeignProposal(t, candidate, "node-a")
	commitAs(t, f.store, r.Generation(), "node-a")
	committed, err := f.store.Current(context.Background())
	require.NoError(t, err)

	// This member provisionally applied the generation and then reverted it
	// locally, and its config source has since staged something else.
	f.applier.gate.record(committed.Generation())
	f.applier.revertedGen = committed.Generation()
	f.sup.rollout.stage("a-different-digest", candidate, candidate)

	staged, applied := f.applier.observedState(committed)

	assert.False(t, staged)
	assert.False(t, applied,
		"a member that reverted the generation locally is not running it, whatever its gate mark says")
}

// TestRolloutObserver_ReportsNoDivergenceOnceAConfirmWindowHasExpired keeps the
// fleet alarm off the protocol working as designed. An expired confirm window
// with no coordinator decision is precisely when every member reverts to the last
// confirmed generation on its own deadman — and it is also when the row can sit
// at "committed" for a while, because the coordinator that would resolve it is
// slow or dead. Counting those correctly-reverted members as diverged would page
// for the deadman doing its job.
func TestRolloutObserver_ReportsNoDivergenceOnceAConfirmWindowHasExpired(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))
	rec := &ports.RecordingExporter{}

	boot := soloWindowConfig(0, time.Second)
	boot.Bindings[0].Address = "addr/original"
	host := newFakeRolloutHost(boot)
	host.unconverged[7] = true // the window can only expire

	candidate := soloWindowConfig(7, time.Second)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)

	barrier := &rolloutBarrier{store: store, memberID: "node-a", pollInterval: time.Second, ops: newRolloutOps(0)}
	barrier.stage(digest, candidate, candidate)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(rec, fake, time.Second, "node-a"),
	}
	seedWindowedCommit(t, store, digest, 7, time.Second)

	require.NoError(t, applier.tick(ctx))
	require.Equal(t, 7, host.Config().Version, "precondition: the provisional swap landed")

	fake.Advance(time.Hour) // no coordinator decides; the local deadman fires
	for range 5 {
		require.NoError(t, applier.tick(ctx))
	}
	require.Equal(t, 0, host.Config().Version, "the deadman reverted to the last confirmed generation")

	entries := rec.FindEntries(shared.MetricClusterRolloutDiverged)
	require.NotEmpty(t, entries)
	assert.Zero(t, entries[len(entries)-1].FValue,
		"a member the deadman correctly reverted is not diverged from the cohort")
}

// TestRolloutObserver_RepublishesTheLevelGaugesEveryTick is what makes the fleet
// alarms able to fire at all.
//
// Divergence and terminal are LEVELS: a member is either off the decided
// generation or it is not. Published once, at the moment they change, they cannot
// sustain an alarm — an alarm spanning several evaluation periods fills the empty
// ones, and "missing data is not breaching" is the only sane default for a series
// a deployment without the barrier never emits. A terminal member that emitted a
// single datapoint and went quiet would therefore never page, on precisely the
// condition whose documented remedy is operator action.
func TestRolloutObserver_RepublishesTheLevelGaugesEveryTick(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	obs := newRolloutObserver(rec, fake, time.Second, "node-a")

	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))
	cfg := soloCohortConfig(3)
	seedRollout(t, store, cfg, persistence.RolloutCommitted)
	r, err := store.Current(context.Background())
	require.NoError(t, err)

	obs.observe(r, "node-a", true, false) // committed, and this member is behind
	obs.observeTerminal(1, "cannot reach the committed generation")

	// Further ticks make no new observation — the store is quiet, or the member is
	// simply between rollouts. The levels must keep being reported.
	for range 5 {
		fake.Advance(time.Second)
		obs.publishLevels()
	}

	diverged := rec.FindEntries(shared.MetricClusterRolloutDiverged)
	terminal := rec.FindEntries(shared.MetricClusterRolloutTerminal)
	assert.GreaterOrEqual(t, len(diverged), 5,
		"the divergence level must be republished each tick, not only when it changes")
	assert.GreaterOrEqual(t, len(terminal), 5,
		"so must the terminal level, or a five-period alarm can never fire")
	assert.Equal(t, 1.0, diverged[len(diverged)-1].FValue)
	assert.Equal(t, 1.0, terminal[len(terminal)-1].FValue)
}

// TestRolloutObserver_AnAbsentRowClearsTheLastObservation stops a resolved or
// deleted rollout from leaving a divergence latched forever. "No rollout in
// flight" is a genuine observation, so it refreshes freshness — and if it kept
// the previous row's fields it would report a member as behind a generation that
// no longer exists, with a FRESH timestamp vouching for it.
func TestRolloutObserver_AnAbsentRowClearsTheLastObservation(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	obs := newRolloutObserver(rec, fake, time.Second, "node-a")

	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))
	seedRollout(t, store, soloCohortConfig(3), persistence.RolloutCommitted)
	r, err := store.Current(context.Background())
	require.NoError(t, err)
	obs.observe(r, "node-a", true, false)
	preDegraded, _ := obs.status().DegradedState()
	require.True(t, preDegraded, "precondition: the observation degrades")

	obs.observeAbsent()

	got := obs.status()
	assert.Empty(t, got.State, "an absent row is not a committed one")
	assert.False(t, got.Stale, "the read succeeded; it just found nothing")
	degraded, _ := got.DegradedState()
	assert.False(t, degraded, "a rollout that is gone cannot leave this member degraded")
	assert.Equal(t, "node-a", got.MemberID, "the member still identifies itself")
}

// TestRolloutDivergence_GaugeAndHealthAgree pins the two implementations of one
// rule against each other.
//
// The divergence question is answered twice — once over the observed row for the
// metric, once over the published snapshot for deep health — because the two run
// at different times, on different inputs, in different processes. That is fine
// as long as they cannot disagree: an operator holding a page that says "a member
// is behind" and a health endpoint that says "converged" has no way to tell which
// one is lying. Every state the row can be in is checked here, both ways.
func TestRolloutDivergence_GaugeAndHealthAgree(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	cfg := soloCohortConfig(3)
	digest, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)

	// A store admits one active rollout at a time, so each state gets its own.
	var store *memoryrollout.Store

	rows := map[string]persistence.Rollout{}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) persistence.Rollout
	}{
		{name: "proposed", build: func(t *testing.T) persistence.Rollout {
			t.Helper()
			r, err := store.Propose(context.Background(), persistence.RolloutProposal{
				ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 3,
				Members: []string{"node-a"}, TTL: time.Hour,
			})
			require.NoError(t, err)
			return r
		}},
		{name: "committed", build: func(t *testing.T) persistence.Rollout {
			t.Helper()
			seedCommit(t, store, digest, 3)
			r, err := store.Current(context.Background())
			require.NoError(t, err)
			return r
		}},
		{name: "provisionally committed", build: func(t *testing.T) persistence.Rollout {
			t.Helper()
			seedWindowedCommit(t, store, digest, 3, time.Minute)
			r, err := store.Current(context.Background())
			require.NoError(t, err)
			return r
		}},
		{name: "confirmed", build: func(t *testing.T) persistence.Rollout {
			t.Helper()
			seedWindowedCommit(t, store, digest, 3, time.Minute)
			r, err := store.Current(context.Background())
			require.NoError(t, err)
			confirmRollout(t, store, r.Generation())
			r, err = store.Current(context.Background())
			require.NoError(t, err)
			return r
		}},
		{name: "reverted", build: func(t *testing.T) persistence.Rollout {
			t.Helper()
			seedWindowedCommit(t, store, digest, 3, time.Minute)
			cur, err := store.Current(context.Background())
			require.NoError(t, err)
			require.NoError(t, store.Revert(context.Background(), cur.Generation(),
				persistence.LeaseToken{Owner: "coord", Version: 1}, "the confirm window expired"))
			r, err := store.Current(context.Background())
			require.NoError(t, err)
			return r
		}},
	} {
		store = memoryrollout.NewStore(memoryrollout.WithClock(fake))
		rows[tc.name] = tc.build(t)
	}

	for name, r := range rows {
		for _, applied := range []bool{true, false} {
			obs := &rolloutObserver{}
			obs.observe(r, "node-a", true, applied)

			health, _ := obs.status().DegradedState()
			assert.Equal(t, rolloutDiverged(r, applied), obs.snap.divergedFromCohort(),
				"%s / applied=%v: the gauge and the health rule must answer alike", name, applied)
			if rolloutDiverged(r, applied) {
				assert.True(t, health, "%s / applied=%v: a diverged member must degrade", name, applied)
			}
		}
	}
}
