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
// document happens to hold, a rollback converges the cohort, and a member that
// cannot converge inside the confirm window takes the WHOLE cohort back to the
// last confirmed generation instead of leaving it split.
//
// Because the emulator runs each ECS task definition as a real container, it
// also proves the synthesized shape WIRES identity correctly — one single-task
// service per slot, the id baked into that slot's own task definition. It does
// NOT prove that AWS ECS hands a replacement task that identity. That still
// rests on the construct's synth assertions and on the credentialed test the
// static-slot chunk left runnable. Any published claim must say which of the
// two it rests on.
func TestLocal_StaticSlotCohort(t *testing.T) {
	env := RequireSandbox(t)
	cohort := DeployLocalCohort(t, env, staticSlotRoster())

	// Budget: the shared phase waits are 15 minutes each and this test runs
	// seven of them plus two shorter ones, so the parent must exceed their sum
	// or a slow-but-correct run dies inside whatever poll happened to be running
	// and the failure names an unrelated phase.
	ctx, cancel := context.WithTimeout(context.Background(), 95*time.Minute)
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

	t.Run("confirm_window_reverts_the_whole_cohort", func(t *testing.T) {
		requireConverged(t)
		// Take one slot away and leave it away. The barrier's roster is frozen
		// from the config, so the absent member can never confirm, and the
		// confirm window must return every remaining member to the last
		// confirmed generation rather than leaving the cohort split across two.
		cohort.ScaleMember(t, ctx, staticSlotWorkerB, 0)
		t.Cleanup(func() { cohort.ScaleMember(t, context.WithoutCancel(ctx), staticSlotWorkerB, 1) })

		survivors := []string{staticSlotControlID, staticSlotWorkerA}
		waitForEverySlot(t, ctx, probe, adminKey, survivors)

		commitLogLevel(t, ctx, probe, controlHost, adminKey, "warn")
		// The state a cohort is in the instant a commit is proposed is the same
		// state it settles back into when the proposal is abandoned, so the
		// revert assertion is vacuous unless the proposal is observed first.
		waitProposalObserved(t, ctx, probe, adminKey, survivors, committed, 6*time.Minute)
		waitCohortAtGeneration(t, ctx, probe, adminKey, survivors, committed, 6*time.Minute)
	})
}
