package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// rolloutDecoded normalizes cfg through the config codec (Marshal -> Parse) so its
// plugin configs are DECODED — the shape a real config source always produces.
// overlayWithRoute builds configs with undecoded (nil) plugin payloads, whose
// canonical projection differs from the decoded form; the committed-artifact
// round-trip only round-trips the decoded form (as production does), so tests that
// exercise the artifact must feed decoded configs to be production-faithful.
func rolloutDecoded(t *testing.T, cfg *ports.BridgeConfig) *ports.BridgeConfig {
	t.Helper()
	encode, decode := rolloutRealCodec()
	raw, err := encode(cfg)
	if err != nil {
		t.Fatalf("normalize encode: %v", err)
	}
	got, err := decode(raw)
	if err != nil {
		t.Fatalf("normalize decode: %v", err)
	}
	return got
}

// Coordinated cluster rollout — the durable last-committed config artifact
// (design Phase-4 residual, closed in Phase 5A) against REAL DynamoDB and the
// REAL config codec (parser.MarshalBridgeConfigJSON <-> parser.Parse). The unit
// tests prove the joiner/applier logic over a fake codec; what only this layer
// proves is that the artifact round-trips a real config THROUGH DynamoDB and back
// with a digest the boot path accepts — the one Phase-5A risk a fake codec cannot
// exercise. A member that boots on the committed config after an abort is passing
// that digest check end to end.

// TestClusterRolloutDDB_CommitWritesCommittedArtifact proves a commit durably
// records the committed config artifact, and that the REAL codec round-trips it:
// the bytes DynamoDB stored decode back to the committed config.
func TestClusterRolloutDDB_CommitWritesCommittedArtifact(t *testing.T) {
	stores := newRolloutCohortStores(t)
	s, changes, swaps, _ := rolloutMember(t, "node-a", stores, rolloutDecoded(t, rolloutCohortConfig("node-a", 0)))

	candidate := rolloutCohortConfig("node-a", 42)
	candidate.Bindings[0].Address = "addr/rolled"
	changes <- rolloutDecoded(t, candidate)
	awaitDeferral(t, swaps)
	wait.Until(t, 20*time.Second, "barrier commits", func() bool {
		return s.Config().Version == 42
	})

	// The durable committed artifact now exists and decodes back to the config.
	wait.Until(t, 5*time.Second, "committed artifact is written", func() bool {
		c, err := stores.rollout.CommittedConfig(context.Background())
		return err == nil && c.ConfigVersion == 42
	})
	committed, err := stores.rollout.CommittedConfig(context.Background())
	if err != nil {
		t.Fatalf("CommittedConfig: %v", err)
	}
	_, decode := rolloutRealCodec()
	got, err := decode(committed.ConfigBytes)
	if err != nil {
		t.Fatalf("the REAL codec must decode the artifact DynamoDB stored: %v", err)
	}
	if got.Version != 42 || got.Bindings[0].Address != "addr/rolled" {
		t.Fatalf("decoded artifact = version %d addr %q, want 42 / addr/rolled", got.Version, got.Bindings[0].Address)
	}
}

// TestClusterRolloutDDB_BootsOnCommittedAfterAbort is the end-to-end proof of the
// Phase-5A residual fix (seq 3) with the real codec: after a rollout aborts, a
// RESTARTED member — a fresh Supervisor over the SAME durable stores, booting on
// the rejected candidate its config source still holds — starts on the last
// COMMITTED config, not the aborted one. This exercises the full path: one member
// wrote the artifact at commit via parser.MarshalBridgeConfigJSON into DynamoDB;
// the restarted member read it back, decoded it via parser.Parse, and PASSED the
// digest recheck (a non-digest-stable round-trip would refuse the boot here).
func TestClusterRolloutDDB_BootsOnCommittedAfterAbort(t *testing.T) {
	stores := newRolloutCohortStores(t)
	a, changesA, swapsA, cancelA := rolloutMember(t, "node-a", stores, rolloutDecoded(t, rolloutCohortConfig("node-a", 0)))

	// 1. Commit a real generation so the durable artifact exists (version 42).
	candidate := rolloutCohortConfig("node-a", 42)
	candidate.Bindings[0].Address = "addr/committed"
	changesA <- rolloutDecoded(t, candidate)
	awaitDeferral(t, swapsA)
	wait.Until(t, 20*time.Second, "barrier commits version 42", func() bool {
		return a.Config().Version == 42
	})

	// 2. Propose a live-safe but UNBUILDABLE candidate (version 43) — it proposes,
	//    this member nacks, and the coordinator aborts. The artifact stays at 42.
	aborting := rolloutCohortConfig("node-a", 43)
	aborting.Bindings[0].Address = "addr/aborted"
	aborting.Routes[0].ReceiverID = "no-such-receiver" // unbuildable -> nack -> abort
	aborting = rolloutDecoded(t, aborting)
	changesA <- aborting
	awaitDeferral(t, swapsA)
	wait.Until(t, 20*time.Second, "the barrier aborts version 43", func() bool {
		st, ok := a.RolloutStatus()
		return ok && st.State == "aborted"
	})

	// 3. Restart: stop member A, then start a fresh Supervisor over the SAME stores
	//    whose boot config is the rejected candidate (what the config source still
	//    holds after the abort). It must boot on the committed config (42), not 43.
	cancelA()

	aRestart, _, _, _ := rolloutMember(t, "node-a", stores, aborting)
	if v := aRestart.Config().Version; v != 42 {
		t.Fatalf("restarted member booted on version %d, want 42 (the committed config, not the aborted candidate 43)", v)
	}
	if got := aRestart.Config().Bindings[0].Address; got != "addr/committed" {
		t.Fatalf("restarted member address = %q, want addr/committed (the committed config)", got)
	}
}
