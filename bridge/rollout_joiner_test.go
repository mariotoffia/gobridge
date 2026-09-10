package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// joinerSupervisor builds a NON-running Supervisor with the barrier wired, so
// the joiner rule can be exercised as the pure boot-time gate it is.
func joinerSupervisor(store ports.ClusterRolloutStore) *Supervisor {
	return newTestSupervisor(WithClusterRollout(testRolloutConfig(store, "node-a")))
}

// seedRollout opens a rollout for cfg's digest and drives it to state.
func seedRollout(t *testing.T, store *memoryrollout.Store, cfg *ports.BridgeConfig, state persistence.RolloutState) {
	t.Helper()
	digest, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: cfg.Version,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	tok := persistence.LeaseToken{Owner: "coord", Version: 1}
	switch state {
	case persistence.RolloutCommitted:
		require.NoError(t, store.Ack(context.Background(), r.Generation(), "node-a", "build"))
		require.NoError(t, store.Commit(context.Background(), r.Generation(), tok))
	case persistence.RolloutAborted:
		require.NoError(t, store.Abort(context.Background(), r.Generation(), tok, "a peer nacked"))
	default:
		// leave it Proposed
	}
}

// TestJoinerRule_InertOutsideCoordinatedMode proves the gate costs nothing to
// every deployment that did not opt in. A standalone bridge, or a clustered one
// under ADR 0012, must boot exactly as before.
func TestJoinerRule_InertOutsideCoordinatedMode(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	seedRollout(t, store, cfg, persistence.RolloutAborted)

	t.Run("no barrier wired", func(t *testing.T) {
		s := newTestSupervisor()
		require.NoError(t, s.checkCoordinatedRolloutPreflight(context.Background(), cfg))
	})
	t.Run("deployment is not coordinated", func(t *testing.T) {
		s := joinerSupervisor(store)
		require.NoError(t, s.checkCoordinatedRolloutPreflight(context.Background(), quickCfg("r1")))
	})
}

// TestJoinerRule_AllowsBootWhenNothingWasEverProposed proves the bootstrap case:
// the very first start of a coordinated cohort has an empty rollout store, and
// an empty store says nothing has been rejected.
func TestJoinerRule_AllowsBootWhenNothingWasEverProposed(t *testing.T) {
	s := joinerSupervisor(memoryrollout.NewStore())
	require.NoError(t, s.checkRolloutJoinerRule(context.Background(), coordinatedClusteredCfg("r1")))
}

// TestJoinerRule_AllowsTheCommittedConfig proves the ordinary restart: the boot
// config is exactly what the cohort committed, so the node joins on it.
func TestJoinerRule_AllowsTheCommittedConfig(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutCommitted)

	s := joinerSupervisor(store)
	require.NoError(t, s.checkRolloutJoinerRule(context.Background(), cfg))
}

// TestJoinerRule_RefusesAnAbortedConfig is the reason the rule exists. Under the
// chosen candidate transport the operator's change is durably in the config
// source BEFORE the barrier decides, so after an ABORT the source still holds
// the rejected document while the cohort keeps running the previous one. A node
// restarting then would boot alone on a config nobody else runs — the
// mixed-version cohort forbids, and a safety REGRESSION against today's
// blanket refusal. It must fail closed instead.
func TestJoinerRule_RefusesAnAbortedConfig(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutAborted)

	s := joinerSupervisor(store)
	err := s.checkRolloutJoinerRule(context.Background(), cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ABORTED")
	assert.Contains(t, err.Error(), "Roll the config source back",
		"the refusal must tell the operator how to recover")
}

// TestJoinerRule_RefusesAnUndecidedConfig covers the narrower window: a node
// restarting between the durable config write and the barrier's decision would
// apply the candidate ahead of the all-member commit — exactly what the barrier
// exists to prevent. Waiting is bounded by the rollout deadline.
func TestJoinerRule_RefusesAnUndecidedConfig(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutProposed)

	s := joinerSupervisor(store)
	err := s.checkRolloutJoinerRule(context.Background(), cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNDECIDED")
}

// TestJoinerRule_AllowsBootWhileAnUnrelatedRolloutIsInFlight proves the rule is
// narrow. A member restarting mid-rollout boots the OLD (committed) config and
// then votes on the candidate through the applier — refusing here would make
// every rollout a cohort-wide restart hazard.
func TestJoinerRule_AllowsBootWhileAnUnrelatedRolloutIsInFlight(t *testing.T) {
	store := memoryrollout.NewStore()
	candidate := coordinatedClusteredCfg("r1")
	candidate.Bindings[0].Address = "addr/candidate"
	candidate.Version = 8
	seedRollout(t, store, candidate, persistence.RolloutProposed)

	booting := coordinatedClusteredCfg("r1") // the older, still-committed document
	s := joinerSupervisor(store)
	require.NoError(t, s.checkRolloutJoinerRule(context.Background(), booting))
}

// TestJoinerRule_AllowsWholeCohortReplacement proves the rule does not break the
// escape hatch it must coexist with. A replacement-required delta (§8) is rolled
// by stopping every member, writing the new config, and starting them again —
// a document no rollout ever carried. Requiring the boot config to EQUAL the last
// committed digest would wedge that documented procedure, so the rule refuses
// only what the barrier explicitly did not commit.
func TestJoinerRule_AllowsWholeCohortReplacement(t *testing.T) {
	store := memoryrollout.NewStore()
	old := coordinatedClusteredCfg("r1")
	old.Version = 7
	seedRollout(t, store, old, persistence.RolloutCommitted)

	replacement := coordinatedClusteredCfg("r1")
	replacement.Stores.Lease = &ports.StoreConfig{Type: "memory"} // replacement-required
	replacement.Version = 8

	s := joinerSupervisor(store)
	require.NoError(t, s.checkRolloutJoinerRule(context.Background(), replacement),
		"whole-cohort replacement must still be able to boot the cohort")
}

// TestJoinerRule_FailsClosedOnStoreError proves an unreadable rollout store
// stops the boot rather than guessing. The store is a hard requirement of
// coordinated mode — a member cannot participate in the barrier without it — so
// "cannot tell committed from rejected" must not resolve to "start anyway".
func TestJoinerRule_FailsClosedOnStoreError(t *testing.T) {
	s := joinerSupervisor(&failingRolloutStore{err: errors.New("dynamodb unavailable")})

	err := s.checkRolloutJoinerRule(context.Background(), coordinatedClusteredCfg("r1"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Refusing to start")
}

// TestSupervisorRun_JoinerRuleBlocksStartup proves the gate is actually wired
// into Run and runs BEFORE anything is built: a refused boot returns the error
// and leaves no runtime behind.
func TestSupervisorRun_JoinerRuleBlocksStartup(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutAborted)

	s := joinerSupervisor(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Run(ctx, cfg, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ABORTED")
	assert.Nil(t, s.Runtime(), "nothing may be built when the joiner rule refuses the boot config")
}
