package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Confirm window (design §8.1) orchestration tests: the coordinator's post-commit
// decision logic (pure) and the whole drive over the controllable fake host, where
// convergence is injectable so the deadman-revert path is deterministic.

// windowedRollout builds a provisionally-committed rollout over epoch with the
// given confirm deadline and the listed members converged.
func windowedRollout(t *testing.T, epoch []string, deadline time.Time, converged ...string) persistence.Rollout {
	t.Helper()
	p := persistence.RolloutProposal{
		ProposerID: "coordinator", ConfigDigest: "digest", ConfigVersion: 7,
		Members: epoch, TTL: 5 * time.Minute, ConfirmWindow: 90 * time.Second,
	}
	r, err := persistence.NewRollout(1, p, rolloutBase.Add(5*time.Minute))
	require.Nil(t, err)
	for _, m := range epoch {
		r, err = r.WithAck(m, "build:"+m, rolloutBase)
		require.Nil(t, err)
	}
	r, err = r.WithProvisionalCommit(persistence.LeaseToken{Owner: "coordinator", Version: 1}, deadline)
	require.Nil(t, err)
	for _, m := range converged {
		r, err = r.WithConverged(m, rolloutBase)
		require.Nil(t, err)
	}
	return r
}

func TestDecideRollout_ConfirmWindow(t *testing.T) {
	epoch := []string{"node-a", "node-b"}
	deadline := rolloutBase.Add(90 * time.Second)

	t.Run("all converged commits to confirm", func(t *testing.T) {
		r := windowedRollout(t, epoch, deadline, "node-a", "node-b")
		action, _ := decideRollout(r, epoch, rolloutBase.Add(time.Second))
		assert.Equal(t, rolloutActionConfirm, action)
	})

	t.Run("within deadline and incomplete waits", func(t *testing.T) {
		r := windowedRollout(t, epoch, deadline, "node-a") // only 1 of 2 converged
		action, _ := decideRollout(r, epoch, rolloutBase.Add(time.Second))
		assert.Equal(t, rolloutActionWait, action)
	})

	t.Run("past deadline and incomplete reverts", func(t *testing.T) {
		r := windowedRollout(t, epoch, deadline, "node-a")
		action, reason := decideRollout(r, epoch, deadline.Add(time.Second))
		assert.Equal(t, rolloutActionRevert, action)
		assert.Contains(t, reason, "confirm window expired")
	})

	t.Run("past deadline reverts even if all converged", func(t *testing.T) {
		// The confirmation must land WITHIN the window: past the deadline every
		// member's local deadman is already reverting to N-1, so a late coordinator
		// must revert to match — confirming here would flap the cohort (adversarial
		// review finding). This is NETCONF confirmed-commit's "abort by inaction".
		r := windowedRollout(t, epoch, deadline, "node-a", "node-b")
		action, _ := decideRollout(r, epoch, deadline.Add(time.Second))
		assert.Equal(t, rolloutActionRevert, action)
	})
}

// soloWindowConfig is a solo-cohort coordinated config carrying a confirm window.
func soloWindowConfig(version int, window time.Duration) *ports.BridgeConfig {
	cfg := soloCohortConfig(version)
	cfg.Bridge.Cluster.ConfirmWindow = window.String()
	return cfg
}

// TestClusterRolloutDriver_ConfirmWindow_HappyPath drives the full confirm window
// over the fake host: propose -> provisional commit -> converge -> confirm, ending
// with the host running the new config and the rollout Confirmed.
func TestClusterRolloutDriver_ConfirmWindow_HappyPath(t *testing.T) {
	store := memoryrollout.NewStore()
	boot := soloWindowConfig(0, 5*time.Second)
	host := newFakeRolloutHost(boot)

	rc := fastRolloutConfig(store, "node-a")
	rc.Encode = newConfigCodecFake().encode // wire the artifact codec so the confirm advances it
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop()

	candidate := soloWindowConfig(7, 5*time.Second)
	candidate.Bindings[0].Address = "addr/confirmed"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 5*time.Second, "the confirm window confirms and the host stays on the new config", func() bool {
		st, ok := d.Status()
		return ok && st.State == string(persistence.RolloutConfirmed)
	})
	assert.Equal(t, 7, host.Config().Version, "the confirmed generation is the running config")
	assert.Equal(t, "addr/confirmed", host.Config().Bindings[0].Address)

	// The durable committed artifact advanced to the confirmed generation, so a
	// reboot now boots on it (not the provisional one).
	cs, ok := store.CommittedConfig(context.Background())
	require.NoError(t, ok)
	assert.Equal(t, uint64(1), cs.Generation)
	assert.Equal(t, 7, cs.ConfigVersion)
}

// TestClusterRolloutDriver_ConfirmWindow_DeadmanRevert is UC-CR9 in-process: a
// member provisionally swaps but never converges, so the confirm window expires
// and the whole (solo) cohort reverts to the last confirmed generation (N-1). The
// host ends on the ORIGINAL config, and the rollout is Reverted.
func TestClusterRolloutDriver_ConfirmWindow_DeadmanRevert(t *testing.T) {
	store := memoryrollout.NewStore()
	boot := soloWindowConfig(0, 60*time.Millisecond)
	boot.Bindings[0].Address = "addr/original"
	host := newFakeRolloutHost(boot)
	// The candidate version never reaches convergence — the confirm window's raison
	// d'être: a config that builds and swaps but cannot reach broker readiness.
	host.unconverged[7] = true

	rc := fastRolloutConfig(store, "node-a")
	rc.Encode = newConfigCodecFake().encode // a codec IS wired, so "no artifact" proves the revert, not a missing codec
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop()

	candidate := soloWindowConfig(7, 60*time.Millisecond)
	candidate.Bindings[0].Address = "addr/never-converges"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 5*time.Second, "the confirm window expires and the cohort reverts", func() bool {
		st, ok := d.Status()
		return ok && st.State == string(persistence.RolloutReverted)
	})

	assert.Equal(t, 0, host.Config().Version, "the cohort reverted to the last confirmed generation")
	assert.Equal(t, "addr/original", host.Config().Bindings[0].Address, "the original config serves again")

	// The provisional generation must NOT have advanced the durable committed
	// artifact — a reboot must land on the last CONFIRMED generation, never a
	// reverted provisional one.
	_, err := store.CommittedConfig(context.Background())
	assert.Error(t, err, "a reverted rollout must not write a committed artifact")
}
