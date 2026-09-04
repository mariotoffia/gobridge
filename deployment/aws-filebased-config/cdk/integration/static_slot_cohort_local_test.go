//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// The static member-slot rollout proof, executed on a locally emulated
// deployment instead of a credentialed AWS sandbox.
//
// What this run proves is the RUNTIME contract of the coordinated rollout
// barrier on a deployed cohort: distinct restart-stable member ids reach the
// barrier, one live-safe delta is proposed once and committed once, every
// member applies it, a member whose task is replaced rejoins under the SAME id
// on the COMMITTED generation rather than on whatever the shared config
// document happens to hold, a rollback converges the cohort, and a change one
// member cannot take is applied by nobody rather than by some.
//
// It covers both ways a change can fail, which the barrier resolves at
// different points and must not confuse. A member that cannot ANSWER is resolved
// at the vote: the rollout aborts and nobody applies anything. A change every
// member ACCEPTS and none can RUN should be resolved by the confirm window,
// which takes the whole cohort back to its last confirmed generation. The lever
// for the second is a subscription that asks for a QoS the broker caps below what
// was requested — every member builds and validates it, and no member's
// subscriptions are ever satisfied.
//
// The second phase also proves the half that used to be broken on the way: a
// subscription change, the one delta that reaches the barrier through a
// receiver's typed plugin options, is agreed by the whole cohort rather than by
// the member that proposed it and nobody else.
//
// Because the emulator runs each ECS task definition as a real container, it
// also proves the synthesized shape WIRES identity correctly — one single-task
// service per slot, the id baked into that slot's own task definition. It does
// NOT prove that AWS ECS hands a replacement task that identity. That still
// rests on the construct's synth assertions and on the credentialed test that
// deploys the same fixture against a real account. Any published claim must say
// which of the two it rests on.
func TestLocal_StaticSlotCohort(t *testing.T) {
	env := RequireSandbox(t)
	cohort := DeployLocalCohort(t, env, staticSlotRoster())

	// Budget: the shared phase waits are 15 minutes each and this test runs
	// seven of them plus four shorter ones, so the parent must exceed their sum
	// or a slow-but-correct run dies inside whatever poll happened to be running
	// and the failure names an unrelated phase.
	ctx, cancel := context.WithTimeout(context.Background(), 115*time.Minute)
	defer cancel()

	roster := strings.Split(cohort.Outputs["MemberSlotIDs"], ",")
	sort.Strings(roster)
	want := []string{staticSlotControlID, staticSlotWorkerA, staticSlotWorkerB}
	sort.Strings(want)
	if strings.Join(roster, ",") != strings.Join(want, ",") {
		t.Fatalf("deployed roster = %v, want %v", roster, want)
	}
	if strings.TrimSpace(cohort.Outputs["RolloutTableName"]) == "" {
		t.Fatal("the deployment published no rollout coordination table name")
	}

	probe := cohort.Probe()
	adminKey := cohort.AdminKey

	// Every slot must be up, distinct, and already carrying the deployment's
	// generation-zero baseline: without it a restart in the window before the
	// first rollout would boot whatever the shared config document holds.
	slots := waitForEverySlot(t, ctx, probe, adminKey, roster)
	baselines := map[string]struct{}{}
	for id, health := range slots {
		if health.BaselineDigest == "" {
			t.Fatalf("slot %s reports no committed baseline; a restart before the first rollout would not "+
				"recover to the config this deployment admitted", id)
		}
		baselines[health.BaselineDigest] = struct{}{}
	}
	if len(baselines) != 1 {
		t.Fatalf("slots recovered to %d different baselines, want one cohort artifact: %v",
			len(baselines), baselines)
	}

	controlHost := cohort.MemberHost(t, ctx, staticSlotControlID)
	baseGeneration := slots[staticSlotControlID].Generation
	committed := baseGeneration

	// Each phase asserts about the generation the previous one reached, so a
	// phase that runs after a failed predecessor is asserting about a generation
	// the cohort is already sitting on — it would report PASS having proved
	// nothing. converged carries that dependency explicitly.
	converged := false
	requireConverged := func(t *testing.T) {
		t.Helper()
		if !converged {
			t.Skip("the cohort has not converged on a generation, so this phase would assert about a " +
				"generation it is already sitting on")
		}
	}

	t.Run("propose_and_converge", func(t *testing.T) {
		commitLogLevel(t, ctx, probe, controlHost, adminKey, "debug")
		committed = waitCohortApplied(t, ctx, probe, adminKey, roster, baseGeneration)
		converged = true
		t.Logf("cohort converged on generation %d", committed)
	})

	t.Run("member_restart_keeps_its_seat", func(t *testing.T) {
		requireConverged(t)
		// The member id belongs to the deployment, not to the task, so the
		// replacement must rejoin the SAME cohort seat — and its boot
		// resolution must hand it the committed generation.
		cohort.ReplaceMemberTask(t, ctx, staticSlotWorkerA)

		restarted := waitSlotAtGeneration(t, ctx, probe, adminKey, staticSlotWorkerA, committed, 6*time.Minute)
		// Re-check identity uniqueness AFTER the replacement: this is exactly
		// when a non-stable identity shows up, as a fourth id or a collapsed seat.
		waitForEverySlot(t, ctx, probe, adminKey, roster)
		if restarted.MemberID != staticSlotWorkerA {
			t.Fatalf("restarted slot announced member_id %q, want %q: the seat is not restart-stable",
				restarted.MemberID, staticSlotWorkerA)
		}
		if !restarted.Applied {
			t.Fatalf("restarted slot %s is at generation %d but has not applied it (%s)",
				staticSlotWorkerA, restarted.Generation, restarted.TerminalReason)
		}
	})

	t.Run("rollback_converges_the_cohort", func(t *testing.T) {
		requireConverged(t)
		converged = false
		commitLogLevel(t, ctx, probe, controlHost, adminKey, "info")
		rolledBack := waitCohortApplied(t, ctx, probe, adminKey, roster, committed)
		committed = rolledBack
		converged = true
		t.Logf("cohort converged on rollback generation %d", rolledBack)
	})

	t.Run("a_change_one_member_cannot_take_is_applied_by_nobody", func(t *testing.T) {
		requireConverged(t)
		// Take one slot away and leave it away. The barrier freezes the roster
		// from the config, so the absent member can never answer for the change
		// — and the whole point of the barrier is that the remaining members
		// then keep running what they were running, together, instead of one
		// applying the change and another not.
		cohort.ScaleMember(t, ctx, staticSlotWorkerB, 0)
		t.Cleanup(func() { cohort.ScaleMember(t, context.WithoutCancel(ctx), staticSlotWorkerB, 1) })

		survivors := []string{staticSlotControlID, staticSlotWorkerA}
		waitForEverySlot(t, ctx, probe, adminKey, survivors)

		commitLogLevel(t, ctx, probe, controlHost, adminKey, "warn")
		// The state a cohort is in the instant a commit is proposed is the same
		// state it ends in when the proposal is abandoned, so the assertion
		// below is vacuous unless the proposal is observed first.
		waitProposalObserved(t, ctx, probe, adminKey, survivors, committed, 6*time.Minute)
		outcome := waitCohortRejected(t, ctx, probe, adminKey, survivors, committed, 6*time.Minute)
		t.Logf("the cohort settled on %q and nobody applied the change", outcome)
	})

	t.Run("the_confirm_window_takes_back_a_change_nobody_can_run", func(t *testing.T) {
		requireConverged(t)
		converged = false
		// The phase before this one took a member away and put it back; the whole
		// cohort has to be answering again before a proposal that needs every
		// member's vote is made.
		waitForEverySlot(t, ctx, probe, adminKey, roster)
		// A subscription asking for QoS 2 against a broker capped at QoS 1. Every
		// member validates and builds it — the vote is a build, and nothing about
		// this config is unbuildable — and no member can then actually run it: the
		// broker grants the filter one level below what was asked for, the reconcile
		// fails, and the session restarts into the same verdict.
		//
		// It is the one change that reaches the barrier through a receiver's typed
		// plugin options, which is why it is also the proof that a subscription
		// change can be agreed at all: it used to be acknowledged by the member that
		// proposed it and by nobody else.
		// Baseline on where the cohort IS, not on the last generation it
		// COMMITTED. The phase before this one aborted a proposal, which leaves a
		// settled generation in the shared row that no member applied; a wait
		// keyed to the committed generation is satisfied by that one the instant
		// it is asked, and reports its acknowledgements as if they were this
		// phase's.
		proposed := currentCohortGeneration(t, ctx, probe, adminKey, roster)
		commitOverlay(t, ctx, probe, controlHost, adminKey, map[string]any{
			"receivers": []map[string]any{{
				"id":     haReceiverID,
				"topics": []map[string]any{{"topic": haProbeTopic, "qos": 2}},
			}},
		})

		// The confirm window is 90s and a member's local deadman waits a few poll
		// intervals past it, so this budget has to clear both plus the coordinator's
		// own cadence.
		outcome := waitCohortSettled(t, ctx, probe, adminKey, roster, proposed, 12*time.Minute)
		if len(outcome.Acked) != len(roster) {
			t.Fatalf("generation %d settled on %d/%d acks (%v); a subscription change reaches the "+
				"barrier through a receiver's typed plugin options, and every member has to be able "+
				"to agree on it",
				outcome.Generation, len(outcome.Acked), len(roster), outcome.Acked)
		}
		t.Logf("generation %d was acked by the whole cohort (%v) and settled on %q",
			outcome.Generation, outcome.Acked, outcome.State)

		if outcome.State != rolloutStateReverted {
			t.Fatalf("the cohort settled on %q, want %q. %q would mean it KEPT a change no member can "+
				"run — the outcome the confirm window exists to prevent, and the one it produced while "+
				"a member was allowed to record convergence over a session it had not re-established. "+
				"%q would mean the vote refused the change and the window was never exercised at all",
				outcome.State, rolloutStateReverted, rolloutStateConfirmed, rolloutStateAborted)
		}
		t.Logf("the cohort reverted generation %d to its last confirmed generation", outcome.Generation)
	})
}
