package main

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeApplyNotifier records NotifyApplyResult calls so the test can assert the
// exact config pointer and error the pipeline forwarded (XCUT-A).
type fakeApplyNotifier struct {
	calls []notifyCall
}

type notifyCall struct {
	cfg *ports.BridgeConfig
	err error
}

func (f *fakeApplyNotifier) NotifyApplyResult(cfg *ports.BridgeConfig, err error) {
	f.calls = append(f.calls, notifyCall{cfg: cfg, err: err})
}

// XCUT-A: the composition root must report DEFINITIVE reconfiguration outcomes
// back to the desired-config manager, passing the EXACT ev.NewConfig pointer
// (the manager correlates by pointer identity) and the swap error verbatim so
// desired-vs-running divergence clears on success and is raised on failure.
func TestReloadPipeline_OnSwap_NotifiesDefinitiveApplyResults(t *testing.T) {
	notifier := &fakeApplyNotifier{}
	p := newReloadPipeline(ports.NewRegistry(), discardLogger(), withApplyResultNotifier(notifier))

	okCfg := testConfig("bridge-demo", 1, "info")
	p.onSwap(bridge.SwapEvent{NewConfig: okCfg, Error: nil})

	failCfg := testConfig("bridge-demo", 2, "info")
	boom := errors.New("swap failed")
	p.onSwap(bridge.SwapEvent{NewConfig: failCfg, Error: boom})

	if len(notifier.calls) != 2 {
		t.Fatalf("expected 2 NotifyApplyResult calls, got %d", len(notifier.calls))
	}

	// A successful definitive apply reports the exact NewConfig pointer, no error.
	if notifier.calls[0].cfg != okCfg {
		t.Errorf("call[0] cfg pointer = %p, want the exact ev.NewConfig %p", notifier.calls[0].cfg, okCfg)
	}
	if notifier.calls[0].err != nil {
		t.Errorf("call[0] err = %v, want nil", notifier.calls[0].err)
	}

	// A failed definitive apply reports the exact NewConfig pointer AND the error.
	if notifier.calls[1].cfg != failCfg {
		t.Errorf("call[1] cfg pointer = %p, want the exact ev.NewConfig %p", notifier.calls[1].cfg, failCfg)
	}
	if !errors.Is(notifier.calls[1].err, boom) {
		t.Errorf("call[1] err = %v, want boom", notifier.calls[1].err)
	}
}

// A DEFERRED event (bridge paused: committed-but-not-applied, Error == nil) is
// NOT a definitive apply, so it must NOT be reported — the manager keeps
// ReconfigurePending until a real apply result lands.
func TestReloadPipeline_OnSwap_SkipsDeferredEvents(t *testing.T) {
	notifier := &fakeApplyNotifier{}
	p := newReloadPipeline(ports.NewRegistry(), discardLogger(), withApplyResultNotifier(notifier))

	deferredCfg := testConfig("bridge-demo", 3, "info")
	p.onSwap(bridge.SwapEvent{NewConfig: deferredCfg, Deferred: true, Error: nil})

	if len(notifier.calls) != 0 {
		t.Fatalf("expected no NotifyApplyResult call for a deferred event, got %d", len(notifier.calls))
	}
}

// With no notifier wired the pipeline must not panic on a swap (deployments
// without divergence tracking).
func TestReloadPipeline_OnSwap_NoNotifierIsNoop(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())
	p.onSwap(bridge.SwapEvent{NewConfig: testConfig("bridge-demo", 1, "info")})
}

// staticLoader / chanWatcher are minimal ports.Loader / ports.Watcher fakes for
// driving a REAL config.Manager through boot + one steady-state emit.
type staticLoader struct{ cfg *ports.BridgeConfig }

func (l staticLoader) Load(context.Context) (*ports.BridgeConfig, error) { return l.cfg, nil }

type chanWatcher struct{ ch <-chan *ports.BridgeConfig }

func (w chanWatcher) Watch(context.Context) (<-chan *ports.BridgeConfig, error) { return w.ch, nil }

// FIX 3 (XCUT-A): after an admin in-band commit applies cfgA, the file watcher
// re-emits the identical content. The config.Manager records THAT re-emit
// pointer as its desiredConfig, so if the pipeline skips the redundant swap
// WITHOUT acking, ReconfigurePending stays true forever for a config the runtime
// already runs. The redundant-skip branch must ack the exact skipped pointer so
// desired-vs-running divergence clears. Verified against a REAL config.Manager so
// the pointer-identity correlation (cfg == m.desiredConfig) is exercised.
func TestReloadPipeline_RedundantFileReload_AcksDesiredConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Boot the manager on v1 and confirm it running so ReconfigurePending is a
	// meaningful signal (it stays false until the first apply result).
	boot := testConfig("bridge-demo", 1, "info")
	watchCh := make(chan *ports.BridgeConfig, 1)
	mgr := config.NewManager(config.Layer{
		Name:    "file",
		Loader:  staticLoader{cfg: boot},
		Watcher: chanWatcher{ch: watchCh},
	})
	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("manager Load: %v", err)
	}
	mgr.NotifyApplyResult(loaded, nil)
	if mgr.ReconfigurePending() {
		t.Fatal("precondition: boot config must be confirmed running (not pending)")
	}

	out, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("manager Watch: %v", err)
	}
	t.Cleanup(mgr.Stop)

	// The manager observes a new desired config (v2) and emits it. It is NOT yet
	// applied through the manager's ack path, so divergence is now pending.
	watchCh <- testConfig("bridge-demo", 2, "info")
	desired := receiveConfig(t, out) // EXACT pointer the manager recorded as desiredConfig
	if !mgr.ReconfigurePending() {
		t.Fatal("precondition: an emitted-but-unacked desired config must be pending")
	}

	reg := ports.NewRegistry()
	p := newReloadPipeline(reg, discardLogger(), withApplyResultNotifier(mgr))
	// The runtime already runs this exact content (an admin commit applied it
	// in-band a moment ago), so the pipeline records its canonical fingerprint
	// and will treat the watcher re-emit as redundant.
	p.recordApplied(desired)

	fileCh := make(chan *ports.BridgeConfig, 2)
	go p.run(ctx, fileCh)

	// Feed the EXACT desired pointer as the watcher's redundant re-emit, then a
	// genuine external edit. run processes the channel sequentially, so once the
	// external edit is forwarded the redundant skip (and its ack) has definitely
	// completed — a deterministic barrier with no sleep.
	external := testConfig("bridge-demo", 3, "warn")
	fileCh <- desired  // redundant -> skipped + acked
	fileCh <- external // forwarded (barrier)

	got := receiveConfig(t, p.changes())
	if fingerprint(got) != fingerprint(external) {
		t.Fatalf("expected the redundant re-emit skipped and the external edit forwarded; got fp %s (external=%s)",
			fingerprint(got), fingerprint(external))
	}

	if mgr.ReconfigurePending() {
		t.Fatal("redundant file reload must ack the desired config so ReconfigurePending clears")
	}
}
