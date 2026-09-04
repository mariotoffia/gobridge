//go:build integration_aws || integration_local
// +build integration_aws integration_local

package integration

import (
	"context"
	"testing"
	"time"
)

// What a deployed cohort must look like at each step of a rollout, expressed as
// conditions rather than as snapshots.
//
// Every one of these reads the rollout block through the same probe the two
// backends supply differently, and every one of them refuses a STALE
// observation first: a member that lost its view of the shared row keeps
// reporting whatever it last saw, and would otherwise satisfy a wait for that
// state forever.

// waitForEverySlot polls until every roster member answers deep health with its
// own member_id, and returns the last observation per slot.
func waitForEverySlot(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
) map[string]slotHealth {
	t.Helper()
	expected := map[string]struct{}{}
	for _, id := range roster {
		expected[id] = struct{}{}
	}
	var found map[string]slotHealth
	var answered int
	err := pollUntil(ctx, 10*time.Second, 15*time.Minute, func() (bool, error) {
		found, answered = observeSlots(ctx, probe, adminKey)
		for _, id := range roster {
			health, ok := found[id]
			if !ok || !health.fresh() {
				return false, nil
			}
		}
		// Set equality, not containment. A member the roster does not list is
		// the other half of the same defect the seat check below catches — it
		// announces an id the barrier counts against nobody — and a phase that
		// has just taken a member away needs to WAIT for it to be gone rather
		// than read the roster as satisfied while it is still voting.
		for id := range found {
			if _, ok := expected[id]; !ok {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		// No member at all is a different failure from one member missing, and it
		// is the one whose cause is never in this process: every task is down or
		// crash-looping, and only the containers' own logs say why.
		if len(found) == 0 {
			t.Fatalf("no member of the roster %v answered at all, so every task is down or "+
				"crash-looping — read the deployed containers' own logs (GOBRIDGE_INT_KEEP=1 keeps "+
				"them) rather than looking for a protocol fault here", roster)
		}
		t.Fatalf("the running cohort never settled on exactly the roster %v with a fresh observation "+
			"from each: last observed %v", roster, keysOf(found))
	}
	if answered != len(found) {
		t.Fatalf("%d running tasks answered but only %d distinct member ids (%v): two tasks share one "+
			"cohort seat, so the roster looks satisfied while a seat is unoccupied", answered, len(found), keysOf(found))
	}
	return found
}

// waitCohortApplied polls until every roster member reports the SAME generation,
// strictly newer than after, with the swap applied and no confirm window still
// open. Returns that generation.
func waitCohortApplied(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
	after uint64,
) uint64 {
	t.Helper()
	var converged uint64
	var last map[string]slotHealth
	err := pollUntil(ctx, 10*time.Second, 15*time.Minute, func() (bool, error) {
		last, _ = observeSlots(ctx, probe, adminKey)
		generation := uint64(0)
		for _, id := range roster {
			health, ok := last[id]
			if !ok || !health.fresh() || !health.Applied || health.ConfirmPending || health.Generation <= after {
				return false, nil
			}
			if generation == 0 {
				generation = health.Generation
			} else if health.Generation != generation {
				return false, nil // still split across generations
			}
		}
		converged = generation
		return true, nil
	})
	if err != nil {
		t.Fatalf("cohort did not converge past generation %d: %+v", after, last)
	}
	return converged
}

// waitCohortRejected polls until every listed member agrees that a proposal past
// after was NOT taken up, and returns the state they agree on.
//
// This is the shape a cohort must end in when one member cannot take a change:
// every remaining member reads the SAME outcome for the SAME proposal, none of
// them applied it, and no confirm window is left open — so all of them are still
// running the config they were running before. A cohort where one member applied
// and another did not is the split this exists to rule out, and it fails here
// rather than being read as "settled".
//
// The generation in a member's health block is the generation of the ROLLOUT it
// last observed, not the generation it is running. That is why this waits for
// agreement plus applied=false rather than for the previous generation number to
// reappear — the number moves on even when nothing was applied.
//
// Agreement alone is not enough, and this is the trap: a proposal that has only
// just been made reads the same on every member — same generation, nothing
// applied, no confirm window — and would satisfy an agreement check the instant
// it was proposed, while it was still perfectly capable of committing a second
// later. So the outcome must be one the rollout cannot move on from.
func waitCohortRejected(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
	after uint64,
	timeout time.Duration,
) string {
	t.Helper()
	var agreed string
	var last map[string]slotHealth
	err := pollUntil(ctx, 5*time.Second, timeout, func() (bool, error) {
		last, _ = observeSlots(ctx, probe, adminKey)
		generation, state := uint64(0), ""
		for _, id := range roster {
			health, ok := last[id]
			if !ok || !health.fresh() || health.ConfirmPending || health.Generation <= after {
				return false, nil
			}
			if health.Applied || !rolloutIsSettled(health.State) {
				return false, nil
			}
			if generation == 0 {
				generation, state = health.Generation, health.State
				continue
			}
			if health.Generation != generation || health.State != state {
				return false, nil // members still disagree about the outcome
			}
		}
		agreed = state
		return true, nil
	})
	if err != nil {
		t.Fatalf("the cohort never agreed that the proposal past generation %d was not taken up; a "+
			"member that applied it while another did not is exactly the split this rules out: %+v",
			after, last)
	}
	return agreed
}

// waitProposalObserved polls until at least one roster member has actually SEEN
// a rollout newer than after — a generation past it, or a confirm window open on
// it.
//
// Every "the cohort ended up here" assertion needs this in front of it, because
// the state a cohort is in immediately after a commit is proposed is
// indistinguishable from the state it settles back into when that proposal is
// abandoned. Without this gate a revert assertion is satisfied by the cohort
// never having observed the proposal at all — including by the barrier never
// writing it, and by the member that was supposed to be missing still being
// there.
func waitProposalObserved(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
	after uint64,
	timeout time.Duration,
) {
	t.Helper()
	var last map[string]slotHealth
	err := pollUntil(ctx, 2*time.Second, timeout, func() (bool, error) {
		last, _ = observeSlots(ctx, probe, adminKey)
		for _, id := range roster {
			health, ok := last[id]
			if !ok || !health.fresh() {
				continue
			}
			if health.Generation > after || health.ConfirmPending {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("no member ever observed a rollout past generation %d, so the proposal never reached the "+
			"barrier and anything asserted about its outcome would be vacuous: %+v", after, last)
	}
}

// waitSlotAtGeneration polls one slot until it reports the given generation.
func waitSlotAtGeneration(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey, memberID string,
	generation uint64,
	timeout time.Duration,
) slotHealth {
	t.Helper()
	var observed slotHealth
	err := pollUntil(ctx, 10*time.Second, timeout, func() (bool, error) {
		observedSlots, _ := observeSlots(ctx, probe, adminKey)
		health, ok := observedSlots[memberID]
		if !ok || !health.fresh() || health.Generation != generation {
			return false, nil
		}
		observed = health
		return true, nil
	})
	if err != nil {
		t.Fatalf("slot %s did not come back on generation %d", memberID, generation)
	}
	return observed
}

// rolloutOutcome is what a windowed rollout settled on, and who voted for it.
type rolloutOutcome struct {
	Generation uint64
	State      string
	Acked      []string
}

// waitCohortSettled polls until every listed member agrees on the SAME terminal
// outcome for the SAME rollout past after, and returns it.
//
// Unlike waitCohortRejected it admits every terminal state, because a windowed
// rollout has three of them and which one it reaches is the assertion rather than
// the precondition: confirmed means the cohort kept the change, reverted means it
// accepted it and took it back, aborted means it never accepted it at all.
// Agreement is still required — a cohort where one member confirmed and another
// reverted is the split the barrier exists to rule out.
// currentCohortGeneration is the highest rollout generation the roster reports
// right now.
//
// A phase that proposes a change baselines its wait on this rather than on the
// last COMMITTED generation. The two differ whenever a preceding phase left a
// settled-but-uncommitted generation in the shared row — an ABORTED proposal is
// exactly that — and every wait here is keyed on "strictly after", so a
// committed-keyed baseline is satisfied immediately by that stale outcome
// instead of by the proposal this phase is about to make.
func currentCohortGeneration(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
) uint64 {
	t.Helper()
	observed, _ := observeSlots(ctx, probe, adminKey)
	highest := uint64(0)
	for _, id := range roster {
		if health, ok := observed[id]; ok && health.fresh() && health.Generation > highest {
			highest = health.Generation
		}
	}
	return highest
}

func waitCohortSettled(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	adminKey string,
	roster []string,
	after uint64,
	timeout time.Duration,
) rolloutOutcome {
	t.Helper()
	var settled rolloutOutcome
	var last map[string]slotHealth
	err := pollUntil(ctx, 2*time.Second, timeout, func() (bool, error) {
		last, _ = observeSlots(ctx, probe, adminKey)
		var agreed rolloutOutcome
		for _, id := range roster {
			health, ok := last[id]
			if !ok || !health.fresh() || health.ConfirmPending || health.Generation <= after {
				return false, nil
			}
			if !rolloutIsSettled(health.State) && health.State != rolloutStateConfirmed {
				return false, nil
			}
			if agreed.Generation == 0 {
				agreed = rolloutOutcome{Generation: health.Generation, State: health.State, Acked: health.Acked}
				continue
			}
			if health.Generation != agreed.Generation || health.State != agreed.State {
				return false, nil // members still disagree about the outcome
			}
		}
		settled = agreed
		return true, nil
	})
	if err != nil {
		t.Fatalf("the cohort never agreed on a terminal outcome for a rollout past generation %d: %+v",
			after, last)
	}
	return settled
}
