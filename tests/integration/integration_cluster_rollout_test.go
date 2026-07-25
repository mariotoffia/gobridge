package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Coordinated cluster rollout against REAL DynamoDB (design §10 integration
// row). The in-package bridge tests drive the same protocol over the memory
// store; what only a real store can prove is that the barrier's conditional
// writes — the single-winner Propose, the fenced Commit, the ack CAS — behave
// the same when they are actual DynamoDB conditional expressions rather than a
// mutex. A protocol that is correct against a map and wrong against DynamoDB
// looks identical in unit tests.

// rolloutTestCohort wires a Supervisor as the sole member of a coordinated
// cohort, backed by a real DynamoDB rollout store and a real DynamoDB
// coordinator lease. A one-member roster is a legitimate cohort shape: the
// barrier's invariant is "acks cover the epoch", which a one-member epoch
// exercises through the same code path. Multi-PROCESS coverage is the
// long-running UC-CR suite.
func rolloutTestCohort(t *testing.T, memberID string) (*bridge.Supervisor, chan *ports.BridgeConfig, chan bridge.SwapEvent) {
	t.Helper()
	client := ddblocal.Client(t)

	rolloutTable := ddblocal.UniqueTable("rollout-int")
	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	if err := rolloutStore.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure rollout table: %v", err)
	}
	ddblocal.CleanupTable(t, client, rolloutTable)

	leaseTable := ddblocal.UniqueTable("rollout-lease-int")
	leaseStore := dynamodblease.NewStore(client, dynamodblease.WithTableName(leaseTable))
	if err := leaseStore.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure lease table: %v", err)
	}
	ddblocal.CleanupTable(t, client, leaseTable)

	swaps := make(chan bridge.SwapEvent, 8)
	s := newCfgTestSupervisor(
		// The blueprint validator is what makes an Ack mean "this member can run
		// it". The applier's build proof is Builder.Plan — the PREPARE phase —
		// and the runtime's route-graph validation runs in COMMIT, so without a
		// validator a dangling reference passes prepare and only fails when the
		// committed generation is applied. Production wires config.Validate here;
		// so does this fixture, or it would be testing a weaker barrier than the
		// one that ships.
		bridge.WithSupervisorBlueprintValidator(config.Validate),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) {
			select {
			case swaps <- ev:
			default:
			}
		}),
		bridge.WithClusterRollout(bridge.ClusterRolloutConfig{
			Store:    rolloutStore,
			Lease:    leaseStore,
			MemberID: memberID,
			// The lease TTL doubles as the coordinator's lock delay, so it bounds
			// how long the first decision takes; the poll interval is how often a
			// member re-reads the rollout row.
			LeaseTTL:     250 * time.Millisecond,
			PollInterval: 50 * time.Millisecond,
			TTL:          2 * time.Minute,
		}),
	)

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := runSupervisorInBackground(context.Background(), s, rolloutCohortConfig(memberID, 0), changes)
	t.Cleanup(func() { cancel(); <-errCh })
	waitForSupervisorRuntime(t, s, errCh, 10*time.Second)
	return s, changes, swaps
}

// rolloutCohortConfig is a clustered config opted into the coordinated barrier,
// whose roster is memberID alone.
func rolloutCohortConfig(memberID string, version int) *ports.BridgeConfig {
	cfg := overlayWithRoute("test-bridge", "r1")
	// The default route policy sends permanent failures and expiries to a DLQ,
	// and a CLUSTERED deployment requires a DISTRIBUTED DLQ store. This test is
	// about the rollout barrier, not DLQ topology, so drop instead — that keeps
	// the cohort clustered (which is what activates the barrier) without pulling
	// a second DynamoDB store into the fixture.
	cfg.Routes[0].Policy = ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Rollout: "coordinated",
		Members: []string{memberID},
	}
	cfg.Version = version
	return cfg
}

// awaitDeferral drains swap events until the barrier deferral arrives, failing
// on any error event on the way. The applier's own commit-time swap also fires
// onSwap, so the channel carries more than deferrals.
func awaitDeferral(t *testing.T, swaps chan bridge.SwapEvent) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-swaps:
			if ev.Error != nil {
				t.Fatalf("swap event carried an error instead of deferring to the barrier: %v", ev.Error)
			}
			if ev.Deferred {
				if ev.DeferReason != bridge.DeferReasonRolloutPending {
					t.Fatalf("defer reason: got %q, want %q", ev.DeferReason, bridge.DeferReasonRolloutPending)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the coordinated rollout deferral")
		}
	}
}

// TestClusterRolloutDDB_HappyPath drives propose → ack → commit → swap entirely
// through DynamoDB conditional writes. Every protocol transition here is a real
// CAS, so a condition expression that is subtly wrong (a missing
// attribute_not_exists, an off-by-one revision) fails here and nowhere else.
func TestClusterRolloutDDB_HappyPath(t *testing.T) {
	s, changes, swaps := rolloutTestCohort(t, "node-a")
	oldRt := s.Runtime()

	candidate := rolloutCohortConfig("node-a", 42)
	candidate.Bindings[0].Address = "addr/rolled"
	changes <- candidate

	awaitDeferral(t, swaps)
	if s.Config().Version != 0 {
		t.Fatalf("config advanced before the barrier committed: version=%d", s.Config().Version)
	}
	if s.Runtime() != oldRt {
		t.Fatal("a member swapped before the all-member commit")
	}

	wait.Until(t, 20*time.Second, "the DynamoDB-backed barrier commits and the member applies", func() bool {
		return s.Config().Version == 42
	})

	if got := s.Config().Bindings[0].Address; got != "addr/rolled" {
		t.Fatalf("applied address: got %q, want addr/rolled", got)
	}
	status, ok := s.RolloutStatus()
	if !ok {
		t.Fatal("a coordinated deployment must publish rollout status")
	}
	if status.State != "committed" {
		t.Fatalf("rollout state: got %q, want committed", status.State)
	}
	if len(status.Acked) != 1 || status.Acked[0] != "node-a" {
		t.Fatalf("acked members: got %v, want [node-a]", status.Acked)
	}
}

// TestClusterRolloutDDB_NackAborts drives the abort path through DynamoDB: a
// member that cannot build the candidate nacks, the fenced coordinator aborts,
// and the running config keeps serving. The store must accept the nack and the
// fenced abort under the same conditional-write rules the commit path uses.
func TestClusterRolloutDDB_NackAborts(t *testing.T) {
	s, changes, swaps := rolloutTestCohort(t, "node-a")
	oldRt := s.Runtime()
	oldAddr := s.Config().Bindings[0].Address

	// Valid enough to be classified live-safe and proposed, but unbuildable: the
	// route references a receiver that does not exist, so prepare fails and the
	// member must nack rather than ack a config it cannot run.
	candidate := rolloutCohortConfig("node-a", 42)
	candidate.Routes[0].ReceiverID = "no-such-receiver"
	changes <- candidate

	// The candidate is live-safe by class, so it IS proposed; the rejection
	// happens where it must — at the member that cannot build it. The barrier
	// then aborts under a real fenced DynamoDB conditional write.
	awaitDeferral(t, swaps)

	wait.Until(t, 20*time.Second, "the barrier aborts on this member's nack", func() bool {
		status, ok := s.RolloutStatus()
		return ok && status.State == "aborted"
	})

	status, _ := s.RolloutStatus()
	if len(status.Nacked) != 1 || status.Nacked[0] != "node-a" {
		t.Fatalf("nacked members: got %v, want [node-a]", status.Nacked)
	}
	if status.Reason == "" {
		t.Fatal("an abort must carry the reason an operator needs to act on")
	}
	if s.Config().Version != 0 {
		t.Fatalf("a rejected candidate must not advance the applied config: version=%d", s.Config().Version)
	}
	if got := s.Config().Bindings[0].Address; got != oldAddr {
		t.Fatalf("running address changed: got %q, want %q", got, oldAddr)
	}
	if s.Runtime() != oldRt {
		t.Fatal("the running runtime must keep serving")
	}
}

// TestClusterRolloutDDB_ReplacementRequiredDeltaIsRefused proves the §8 class
// gate holds against the real store: a change to the cohort's OWN roster is
// never proposed at all, so ADR 0012's whole-cohort replacement stays the
// contract for it.
func TestClusterRolloutDDB_ReplacementRequiredDeltaIsRefused(t *testing.T) {
	s, changes, swaps := rolloutTestCohort(t, "node-a")

	destructive := rolloutCohortConfig("node-a", 42)
	destructive.Bridge.Cluster.Members = []string{"node-a", "node-b"}
	changes <- destructive

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-swaps:
			if ev.Deferred {
				t.Fatal("a replacement-required delta must be refused, never deferred to the barrier")
			}
			if ev.Error == nil {
				continue
			}
			if status, ok := s.RolloutStatus(); ok && status.State != "" {
				t.Fatalf("no rollout may be opened for a replacement-required delta; state=%q", status.State)
			}
			if s.Config().Version != 0 {
				t.Fatalf("the applied config must not advance: version=%d", s.Config().Version)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the replacement-required refusal")
		}
	}
}
