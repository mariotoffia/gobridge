package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestManager_AdoptRunning_ReconcilesABarrierDrivenSwap is the Phase-6
// config-manager reconcile. A coordinated cluster rollout applies a config the
// manager never EMITTED — the applier's frozen clone, or the durable committed
// artifact decoded at boot/reconcile — which is a DIFFERENT pointer than the one
// the manager stamped as desired. NotifyApplyResult correlates by that exact
// pointer, so it drops the barrier's swap as foreign and ReconfigurePending (and,
// downstream, deep-health Degraded) latches true forever even though the member is
// correctly converged. AdoptRunning is the composition-root reconcile: it advances
// the running version/fingerprint to the config actually applied, bypassing the
// emit-pointer gate, so health reflects the real convergence.
func TestManager_AdoptRunning_ReconcilesABarrierDrivenSwap(t *testing.T) {
	v1 := minimalValidConfig("bridge1")
	v1.Version = 1
	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: v1}})
	_, err := mgr.Load(context.Background())
	require.NoError(t, err)
	mgr.NotifyApplyResult(v1, nil)
	require.False(t, mgr.ReconfigurePending(), "boot config confirmed running")

	// The operator writes v2; the manager emits it as the new desired, but the
	// barrier DEFERS the live-safe delta (no local swap), so running is still v1 and
	// the manager is correctly pending.
	v2 := minimalValidConfig("bridge1")
	v2.Version = 2
	mgr.recordAppliedVersion(v2)
	require.True(t, mgr.ReconfigurePending(), "v2 desired, only v1 confirmed running")

	// The barrier commits and this member swaps to v2 — but through a DIFFERENT
	// pointer than the manager emitted (the applier's frozen candidate, or the
	// decoded committed artifact). A plain NotifyApplyResult drops it as foreign, so
	// running stays at v1 and pending stays latched.
	v2applied := minimalValidConfig("bridge1")
	v2applied.Version = 2
	require.NotSame(t, v2, v2applied, "the barrier applies a config the manager did not emit")
	mgr.NotifyApplyResult(v2applied, nil)
	rv, _ := mgr.RunningVersion()
	require.Equal(t, 1, rv, "the emit-pointer gate drops a foreign-pointer ack")
	require.True(t, mgr.ReconfigurePending(), "and so the divergence latches — the gap AdoptRunning closes")

	// AdoptRunning reconciles running to the applied content. Because the applied
	// content equals the desired content, ReconfigurePending clears — the member is
	// running exactly what the source desires.
	mgr.AdoptRunning(v2applied)
	rv, ok := mgr.RunningVersion()
	require.True(t, ok)
	assert.Equal(t, 2, rv, "AdoptRunning advances running to the barrier-applied config")
	assert.False(t, mgr.ReconfigurePending(), "the member is converged; health must not read pending")
	assert.NoError(t, mgr.LastApplyError())
}

// TestManager_AdoptRunning_NilIsIgnored keeps the reconcile safe to call
// unconditionally after any swap: a nil applied config (nothing to reconcile) is a
// no-op, not a panic or a state corruption.
func TestManager_AdoptRunning_NilIsIgnored(t *testing.T) {
	v1 := minimalValidConfig("bridge1")
	v1.Version = 1
	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: v1}})
	_, err := mgr.Load(context.Background())
	require.NoError(t, err)
	mgr.NotifyApplyResult(v1, nil)

	mgr.AdoptRunning((*ports.BridgeConfig)(nil))
	rv, ok := mgr.RunningVersion()
	require.True(t, ok)
	assert.Equal(t, 1, rv, "a nil adopt is inert")
}
