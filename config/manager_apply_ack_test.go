package config

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// optPluginConfig is a test-only ports.PluginConfig with EXPORTED, JSON-visible
// fields (unlike fakePluginConfig, whose only field is unexported and therefore
// invisible to json.Marshal). It lets the fingerprint tests vary a plugin OPTION
// and a plugin SECRET and assert the fingerprint changes even though the
// blueprint tags every plugin Config field json:"-" (finding: plugin options
// omitted from the fingerprint).
type optPluginConfig struct {
	Broker string        `json:"broker,omitempty"`
	Token  shared.Secret `json:"token,omitempty"`
}

func (optPluginConfig) Kind() string    { return "opt" }
func (optPluginConfig) Validate() error { return nil }

// sessionConfig returns a copy of cfg with a single session whose decoded plugin
// Config is pc, so a fingerprint test can change ONLY the plugin option/secret.
func withSessionOption(id string, version int, pc ports.PluginConfig) *ports.BridgeConfig {
	cfg := minimalValidConfig(id)
	cfg.Version = version
	sess := ports.SessionDef{ID: "sess1", Transport: "opt"}
	sess.SetDecoded(pc, nil)
	cfg.Sessions = []ports.SessionDef{sess}
	return cfg
}

// TestManager_Watch_FailedSwap_KeepsManagerConsistentWithRuntime is the HIGH-1
// regression (desired-ahead-of-applied). An operator writes a syntactically
// valid config that passes validation, so the manager commits and EMITS it
// downstream — but the runtime swap then fails and the supervisor recovers the
// previous runtime. Before the fix the manager (and, via lastHash, the file
// source) had already advanced, so it silently reported the new config as the
// live one while traffic was still served by the old runtime.
//
// With the fix the manager separates the DESIRED (emitted) version from the
// RUNNING (confirmed-applied) version: after a failed swap RunningVersion stays
// at the previously-applied version, LastApplyError records why, and
// ReconfigurePending surfaces the desired != applied divergence.
func TestManager_Watch_FailedSwap_KeepsManagerConsistentWithRuntime(t *testing.T) {
	base := minimalValidConfig("bridge1")
	base.Version = 1
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	// Boot: Load emits desired v1; the applier confirms it is running.
	_, err := mgr.Load(context.Background())
	require.NoError(t, err)
	mgr.NotifyApplyResult(base, nil)

	rv, ok := mgr.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 1, rv)
	require.False(t, mgr.ReconfigurePending(), "boot config confirmed running")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	// Operator writes a valid v2; the manager validates and EMITS it downstream
	// (this is the point where, pre-fix, the manager/source got ahead of the
	// runtime).
	v2 := minimalValidConfig("bridge1")
	v2.Version = 2
	watchCh <- v2
	select {
	case got := <-out:
		require.Equal(t, 2, got.Version, "manager emitted the desired v2")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the manager to emit v2")
	}

	dv, ok := mgr.AppliedVersion()
	require.True(t, ok)
	assert.Equal(t, 2, dv, "desired version advanced to the emitted v2")

	// The runtime swap to v2 fails; the supervisor recovers v1. The applier
	// echoes back the exact config it attempted (v2) so the manager correlates
	// the result by content.
	boom := errors.New("route validation rejected v2")
	mgr.NotifyApplyResult(v2, boom)

	// The manager now tracks the ACTUALLY-running runtime (v1), not the desired
	// v2, and reports the divergence rather than claiming the reload took effect.
	rv, ok = mgr.RunningVersion()
	require.True(t, ok)
	assert.Equal(t, 1, rv, "running stays at the recovered v1 after a failed swap")
	assert.True(t, mgr.ReconfigurePending(), "desired v2 != running v1 must be surfaced")
	require.ErrorIs(t, mgr.LastApplyError(), boom)

	// A later successful swap to v2 clears the divergence.
	mgr.NotifyApplyResult(v2, nil)
	rv, _ = mgr.RunningVersion()
	assert.Equal(t, 2, rv)
	assert.False(t, mgr.ReconfigurePending())
	assert.NoError(t, mgr.LastApplyError())
}

// TestManager_NotifyApplyResult_StateMachine pins the desired/applied
// acknowledgement state machine directly (no watch loop): no false "pending"
// before the first result, running advances on success, diverges on failure, and
// results are correlated with the desired config by CONTENT (the applier echoes
// the config it attempted).
func TestManager_NotifyApplyResult_StateMachine(t *testing.T) {
	v5 := minimalValidConfig("bridge1")
	v5.Version = 5
	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: v5}})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	// Desired is surfaced immediately (emitted), but nothing is confirmed yet,
	// so the manager must NOT cry "pending" when no ack path is wired.
	dv, ok := mgr.AppliedVersion()
	require.True(t, ok)
	require.Equal(t, 5, dv)
	_, ok = mgr.RunningVersion()
	require.False(t, ok, "no running version before the first apply result")
	require.False(t, mgr.ReconfigurePending(), "no false pending before any ack")
	require.NoError(t, mgr.LastApplyError())

	// Success confirms the running version and clears any error.
	mgr.NotifyApplyResult(v5, nil)
	rv, ok := mgr.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 5, rv)
	require.False(t, mgr.ReconfigurePending())

	// Manager emits a newer desired v6 (distinct content).
	v6 := minimalValidConfig("bridge1")
	v6.Version = 6
	mgr.recordAppliedVersion(v6)
	require.True(t, mgr.ReconfigurePending(), "v6 desired, only v5 confirmed running")

	// The applier reports the v6 swap FAILED: still pending, and the failure is
	// retained because it pertains to the current desired.
	failed := errors.New("swap rejected")
	mgr.NotifyApplyResult(v6, failed)
	rv, _ = mgr.RunningVersion()
	require.Equal(t, 5, rv, "running unchanged after a failed apply")
	require.True(t, mgr.ReconfigurePending())
	require.ErrorIs(t, mgr.LastApplyError(), failed)

	// Finally v6 applies.
	mgr.NotifyApplyResult(v6, nil)
	rv, _ = mgr.RunningVersion()
	require.Equal(t, 6, rv)
	require.False(t, mgr.ReconfigurePending())
	require.NoError(t, mgr.LastApplyError())
}

// TestManager_ReconfigurePending_ContentChangeWithoutVersionBump is the #2
// regression: desired-vs-running MUST be keyed on config CONTENT, not on the
// operator-controlled (non-unique) BridgeConfig.Version. An external writer
// changes the config CONTENT but leaves version:1 unchanged; the watcher emits
// (content differs) and the manager records the new desired — still version 1.
// The runtime swap then fails and the supervisor keeps the old runtime (also
// version 1). Keyed on Version this looks converged (1 == 1) and the real content
// divergence is HIDDEN; keyed on a content fingerprint it is correctly surfaced.
func TestManager_ReconfigurePending_ContentChangeWithoutVersionBump(t *testing.T) {
	v1 := minimalValidConfig("bridge1")
	v1.Version = 1
	// Same version, DIFFERENT content (InstanceID is merge-only, not validated,
	// so the merged result stays valid) — the exact hazard Version keying misses.
	v1b := minimalValidConfig("bridge1")
	v1b.Version = 1
	v1b.Bridge.InstanceID = "content-changed-but-version-1"

	watchCh := make(chan *ports.BridgeConfig, 1)
	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: v1},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)
	mgr.NotifyApplyResult(v1, nil)
	require.False(t, mgr.ReconfigurePending(), "boot config confirmed running")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	// External writer changes content but not the version; the manager emits it.
	watchCh <- v1b
	select {
	case got := <-out:
		require.Equal(t, 1, got.Version, "version field is unchanged")
		require.Equal(t, "content-changed-but-version-1", got.Bridge.InstanceID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the manager to emit the content change")
	}

	// The swap to the new content fails; the supervisor recovers the old v1.
	boom := errors.New("swap rejected")
	mgr.NotifyApplyResult(v1b, boom)

	dv, _ := mgr.AppliedVersion()
	rv, _ := mgr.RunningVersion()
	require.Equal(t, 1, dv, "desired version field is 1")
	require.Equal(t, 1, rv, "running version field is also 1")
	// The crux: identical Version numbers, but the CONTENT diverged, so a live
	// reload did NOT take effect and the manager must say so. (Pre-fix: keyed on
	// Version, 1 == 1, this returned false and the divergence was hidden.)
	require.True(t, mgr.ReconfigurePending(),
		"content diverged from the running runtime though the version field is unchanged")
	require.ErrorIs(t, mgr.LastApplyError(), boom)

	// A later successful swap to the new content clears the divergence.
	mgr.NotifyApplyResult(v1b, nil)
	require.False(t, mgr.ReconfigurePending())
	require.NoError(t, mgr.LastApplyError())
}

// TestManager_NotifyApplyResult_IgnoresStaleOutOfOrderAck is the #3 regression:
// a late/out-of-order acknowledgement for a config the manager has already
// superseded must NOT regress the running state. A stale SUCCESS ack for v1
// arriving after v2 is confirmed running would (pre-fix, keyed on Version) reset
// runningVersion to 1 and falsely report pending; a stale FAILURE ack for v1
// would leave a stale LastApplyError set even though v2 runs cleanly.
func TestManager_NotifyApplyResult_IgnoresStaleOutOfOrderAck(t *testing.T) {
	v1 := minimalValidConfig("bridge1")
	v1.Version = 1
	v2 := minimalValidConfig("bridge1")
	v2.Version = 2

	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: v1}})
	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	// v1 confirmed running, then v2 emitted and confirmed running.
	mgr.NotifyApplyResult(v1, nil)
	mgr.recordAppliedVersion(v2)
	mgr.NotifyApplyResult(v2, nil)
	rv, _ := mgr.RunningVersion()
	require.Equal(t, 2, rv)
	require.False(t, mgr.ReconfigurePending())

	// A stale SUCCESS ack for the superseded v1 arrives late. It must be ignored:
	// v2 is the desired and running config; regressing to v1 would be wrong.
	mgr.NotifyApplyResult(v1, nil)
	rv, _ = mgr.RunningVersion()
	require.Equal(t, 2, rv, "stale success ack must NOT regress running to v1")
	require.False(t, mgr.ReconfigurePending(), "still converged on v2")

	// A stale FAILURE ack for the superseded v1 arrives late. It must be ignored:
	// v2 is running cleanly, so no error must be surfaced.
	mgr.NotifyApplyResult(v1, errors.New("late boom for v1"))
	require.NoError(t, mgr.LastApplyError(), "stale failure ack must NOT set an error for a superseded config")
	rv, _ = mgr.RunningVersion()
	require.Equal(t, 2, rv)
	require.False(t, mgr.ReconfigurePending())
}

// TestConfigFingerprint_IncludesPluginOptionsAndSecrets is the #1 regression: the
// fingerprint must cover each decoded PluginConfig's options AND secrets, which
// live in the `Config` fields tagged json:"-" on the blueprint. A plain
// json.Marshal of the config drops them, so a same-Version change to a plugin
// option (or a plugin secret) would produce an IDENTICAL fingerprint and a failed
// apply of that change would still read as converged — the original
// hidden-divergence bug, narrowed to plugin options.
func TestConfigFingerprint_IncludesPluginOptionsAndSecrets(t *testing.T) {
	base := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://a:1883"})
	fpBase, err := configFingerprint(base)
	require.NoError(t, err)

	// Same Version, DIFFERENT plugin option.
	optChanged := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://b:1883"})
	fpOpt, err := configFingerprint(optChanged)
	require.NoError(t, err)
	require.NotEqual(t, fpBase, fpOpt,
		"a plugin OPTION change at an unchanged Version must change the fingerprint "+
			"(plugin Config is json:\"-\" and would be dropped by a plain marshal)")

	// Same Version, same option, DIFFERENT plugin secret. The secret redacts to
	// [REDACTED] under a plain marshal, so the fingerprint must be taken over the
	// REVEALED config for a secret-only edit to register.
	secretA := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://a:1883", Token: shared.NewSecret("s1")})
	secretB := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://a:1883", Token: shared.NewSecret("s2")})
	fpSecretA, err := configFingerprint(secretA)
	require.NoError(t, err)
	fpSecretB, err := configFingerprint(secretB)
	require.NoError(t, err)
	require.NotEqual(t, fpSecretA, fpSecretB,
		"a plugin SECRET change at an unchanged Version must change the fingerprint")

	// Determinism: the same config fingerprints identically across calls.
	fpBase2, err := configFingerprint(base)
	require.NoError(t, err)
	require.Equal(t, fpBase, fpBase2, "fingerprint must be deterministic for equal content")
}

// TestManager_ReconfigurePending_PluginOptionChangeWithoutVersionBump is the #4
// regression that #1 makes meaningful: an external writer changes ONLY a plugin
// option and leaves the Version unchanged; the runtime swap fails. Because the
// fingerprint now covers plugin options, the divergence is surfaced. (Pre-#1 fix,
// the plugin option was omitted so both configs fingerprinted equal and this read
// as converged.)
func TestManager_ReconfigurePending_PluginOptionChangeWithoutVersionBump(t *testing.T) {
	v1 := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://a:1883"})
	// Same Version, only the plugin option differs.
	v1b := withSessionOption("bridge1", 1, optPluginConfig{Broker: "tcp://b:1883"})

	watchCh := make(chan *ports.BridgeConfig, 1)
	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: v1},
		Watcher: &stubWatcher{ch: watchCh},
	})

	boot, err := mgr.Load(context.Background())
	require.NoError(t, err)
	mgr.NotifyApplyResult(boot, nil) // boot echoes the emitted pointer
	require.False(t, mgr.ReconfigurePending(), "boot config confirmed running")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	watchCh <- v1b
	var emitted *ports.BridgeConfig
	select {
	case emitted = <-out:
		require.Equal(t, 1, emitted.Version, "version field is unchanged")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the manager to emit the plugin-option change")
	}

	// The swap to the new plugin option fails; the supervisor recovers v1.
	boom := errors.New("swap rejected")
	mgr.NotifyApplyResult(emitted, boom)

	require.True(t, mgr.ReconfigurePending(),
		"a same-Version plugin OPTION change that failed to apply must surface as pending")
	require.ErrorIs(t, mgr.LastApplyError(), boom)
}

// TestManager_ReconfigurePending_UnhashableConfigNotConverged is the #2
// regression: a config that cannot be fingerprinted (here a NaN reached via the
// public ConditionDef.Value any) must NEVER read as converged. Before the fix the
// marshal error collapsed to a zero fingerprint, so two unhashable configs (or an
// unhashable desired against a zero running) compared EQUAL and ReconfigurePending
// falsely returned false though nothing was ever confirmed.
func TestManager_ReconfigurePending_UnhashableConfigNotConverged(t *testing.T) {
	unhashable := minimalValidConfig("bridge1")
	unhashable.Version = 1
	unhashable.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{{
			BindingID: "bind1",
			Match:     []ports.ConditionDef{{Field: "subject", Operator: "eq", Value: math.NaN()}},
		}},
	}

	// configFingerprint must REFUSE (error), not silently return a zero hash.
	_, err := configFingerprint(unhashable)
	require.Error(t, err, "an unserialisable config must not fingerprint to a (zero) value")

	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: unhashable}})
	// recordAppliedVersion is the emit-time stamp; drive it directly (Load would
	// also reach it). The desired is now unhashable.
	mgr.recordAppliedVersion(unhashable)
	require.True(t, mgr.ReconfigurePending(),
		"an unhashable desired can never be proven converged; must read as pending")

	// Even a SUCCESS ack for the unhashable desired must not flip it to converged:
	// we cannot verify the runtime is serving it.
	mgr.NotifyApplyResult(unhashable, nil)
	require.True(t, mgr.ReconfigurePending(),
		"a success ack for an unhashable config must not read as converged")
	require.Error(t, mgr.LastApplyError(), "the unverifiable convergence must be surfaced")

	// Replacing it with a hashable config that is then confirmed clears the state.
	good := minimalValidConfig("bridge1")
	good.Version = 2
	mgr.recordAppliedVersion(good)
	mgr.NotifyApplyResult(good, nil)
	require.False(t, mgr.ReconfigurePending(), "a hashable, confirmed config converges")
	require.NoError(t, mgr.LastApplyError())
}

// TestManager_NotifyApplyResult_FlapDoesNotRegressOnStaleAck is the #3 regression
// for an A→B→A flap. A#1 and A#2 have IDENTICAL content (same fingerprint) but are
// distinct emitted pointers. A delayed SUCCESS ack for A#1 that lands after A#2 was
// emitted must be ignored — correlation is by the exact emitted pointer, not by
// content (content hashing cannot tell A#1 from A#2). Before the fix (content-only
// correlation) the stale A#1 ack matched the current desired A#2 by content and
// set running=fpA, hiding that the runtime was actually still on B.
func TestManager_NotifyApplyResult_FlapDoesNotRegressOnStaleAck(t *testing.T) {
	// A#1, B, A#2 — distinct allocations; A#1 and A#2 share content.
	a1 := minimalValidConfig("bridge1")
	a1.Version = 1
	a1.Bridge.InstanceID = "content-A"
	b := minimalValidConfig("bridge1")
	b.Version = 1
	b.Bridge.InstanceID = "content-B"
	a2 := minimalValidConfig("bridge1")
	a2.Version = 1
	a2.Bridge.InstanceID = "content-A"

	// Sanity: A#1 and A#2 are content-equal (same fingerprint) but different pointers.
	fpA1, err := configFingerprint(a1)
	require.NoError(t, err)
	fpA2, err := configFingerprint(a2)
	require.NoError(t, err)
	require.Equal(t, fpA1, fpA2, "A#1 and A#2 must be content-equal for this test to bite")
	require.NotSame(t, a1, a2, "A#1 and A#2 must be distinct pointers")

	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: a1}})

	// Emit A#1 and confirm it running.
	mgr.recordAppliedVersion(a1)
	mgr.NotifyApplyResult(a1, nil)
	require.False(t, mgr.ReconfigurePending())

	// Emit B and confirm it running.
	mgr.recordAppliedVersion(b)
	mgr.NotifyApplyResult(b, nil)
	require.False(t, mgr.ReconfigurePending(), "runtime confirmed on B")

	// Emit A#2 (desired flips back to A content). The runtime has NOT confirmed it
	// yet — it is still on B — so the manager is pending.
	mgr.recordAppliedVersion(a2)
	require.True(t, mgr.ReconfigurePending(), "A#2 emitted but not yet applied; runtime still on B")

	// A DELAYED success ack for the SUPERSEDED A#1 now arrives. With content-only
	// correlation it would match the current desired (A content) and wrongly mark
	// running=A, hiding that the runtime is on B. Pointer identity rejects it.
	mgr.NotifyApplyResult(a1, nil)
	require.True(t, mgr.ReconfigurePending(),
		"a stale ack for the superseded A#1 must NOT converge A#2 (runtime is still on B)")

	// The real ack for A#2 finally lands and converges.
	mgr.NotifyApplyResult(a2, nil)
	require.False(t, mgr.ReconfigurePending(), "A#2 confirmed running")
}
