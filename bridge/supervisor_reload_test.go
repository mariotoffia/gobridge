package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// Full-session reload semantics (HIGH-10, accepted tradeoff).
//
// The Supervisor detects no-ops on the WHOLE config only. Any accepted delta —
// however narrow — rebuilds the entire runtime: every session, receiver, sender,
// and store is reconstructed and the previous runtime is stopped. Editing one
// route therefore bounces every OTHER route's broker session too, which is a
// fleet-wide connection churn and a loss window for QoS 0 and ephemeral sessions.
//
// Diff-based reload is an explicit non-goal of the production-readiness
// remediation. These tests PIN the retained semantics so the contract published
// to operators (batch config changes; expect a full restart) cannot silently
// drift, and so a future diff-reload implementation must delete this pin
// deliberately rather than by accident.

// twoSessionReloadConfig builds a config with two independent exclusive sessions,
// each driving its own lease-managed route. address2 changes only route r2's
// binding, leaving r1 and session s1 byte-identical between generations.
func twoSessionReloadConfig(version int, address2 string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Version: version,
		Bridge: ports.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}
	for _, r := range []struct{ route, session, address string }{
		{"r1", "s1", "topic/r1"},
		{"r2", "s2", address2},
	} {
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
			ID: r.session, Transport: "exclusive", SessionMode: "exclusive",
		})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{
			ID: r.route + "-rx", Transport: "fake",
		})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{
			ID: r.route + "-tx", Transport: "exclusive", SessionID: r.session,
		})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
			ID: r.route + "-b1", SenderID: r.route + "-tx", SessionID: r.session, Address: r.address,
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID:           r.route,
			ReceiverID:   r.route + "-rx",
			DeliveryMode: "shared_outbox",
			Policy: ports.PolicyDef{
				OnPermanentFailure: "drop",
				OnExpired:          "drop",
			},
			Bindings: []string{r.route + "-b1"},
			Session: &ports.RouteSessionDef{
				SessionID: r.session,
				SenderID:  r.route + "-tx",
			},
		})
	}
	return cfg
}

// TestSupervisorReload_OneRouteChangeRestartsEverySession pins the accepted
// full-session restart semantics: a delta confined to route r2 still tears down
// and rebuilds session s1, which route r2 does not reference.
func TestSupervisorReload_OneRouteChangeRestartsEverySession(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, twoSessionReloadConfig(1, "topic/r2"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	sessions, _, _ := ef.Counts()
	require.Equal(t, 2, sessions, "both configured sessions are built on the initial start")

	// Touch route r2 only. Route r1 and session s1 are unchanged.
	require.True(t, sendConfig(ch, twoSessionReloadConfig(2, "topic/r2-moved"), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)

	sessions, _, _ = ef.Counts()
	assert.Equal(t, 4, sessions,
		"a one-route delta rebuilds EVERY session, not only the route's own: full-session reload is retained")
	assert.False(t, oldRt.IsRunning(),
		"the previous runtime — and with it every unrelated route — is stopped on any accepted reload")
	assert.Equal(t, 2, s.Config().Version)
}

// TestSupervisorReload_NoOpConfigKeepsSessionsRunning is the boundary of the pin:
// the ONLY delta that does not restart every session is a whole-config no-op.
// Anything narrower than "nothing changed" still costs a full restart, which is
// exactly why operators must batch config changes.
func TestSupervisorReload_NoOpConfigKeepsSessionsRunning(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, twoSessionReloadConfig(1, "topic/r2"), ch)
	defer func() { cancel(); <-errCh }()

	rt := s.Runtime()
	require.NotNil(t, rt)

	require.True(t, sendConfig(ch, twoSessionReloadConfig(1, "topic/r2"), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)

	sessions, _, _ := ef.Counts()
	assert.Equal(t, 2, sessions, "a content no-op re-emit builds no replacement session")
	assert.True(t, rt.IsRunning(), "the running runtime is preserved across a no-op re-emit")
}

// TestSupervisorReload_VersionOnlyBumpRestartsEverySession pins the case an
// operator hits by accident: the no-op check canonicalises the WHOLE config, and
// Version is part of it, so a bookkeeping version bump with byte-identical
// content is an accepted delta and costs a full restart like any other.
func TestSupervisorReload_VersionOnlyBumpRestartsEverySession(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, twoSessionReloadConfig(1, "topic/r2"), ch)
	defer func() { cancel(); <-errCh }()

	require.True(t, sendConfig(ch, twoSessionReloadConfig(2, "topic/r2"), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)

	sessions, _, _ := ef.Counts()
	assert.Equal(t, 4, sessions,
		"a version-only bump is not a content no-op; it rebuilds every session")
}

// BenchmarkSupervisorReload_FullSessionRestart measures what the retained
// semantics cost per accepted config change: one full teardown and rebuild of
// every session in the config, driven by a delta that touches one route. It is
// the number an operator's change-batching budget is derived from.
func BenchmarkSupervisorReload_FullSessionRestart(b *testing.B) {
	onSwap, swaps := swapChan(b.N + 1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, twoSessionReloadConfig(1, "topic/r2-0"), ch)
	defer func() { cancel(); <-errCh }()

	b.ResetTimer()
	for i := range b.N {
		cfg := twoSessionReloadConfig(i+2, "topic/r2-"+string(rune('a'+i%26))) //nolint:mnd // rotate the address so no re-emit is a no-op
		if !sendConfig(ch, cfg, 5*time.Second) {
			b.Fatal("config send timed out")
		}
		select {
		case ev := <-swaps:
			if ev.Error != nil {
				b.Fatalf("swap failed: %v", ev.Error)
			}
		case <-time.After(10 * time.Second):
			b.Fatal("swap timed out")
		}
	}
}
