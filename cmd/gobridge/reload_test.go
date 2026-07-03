package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// receiveConfig reads one config from ch, failing the test on timeout.
func receiveConfig(t *testing.T, ch <-chan *ports.BridgeConfig) *ports.BridgeConfig {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a config on the supervisor channel")
		return nil
	}
}

// testConfig builds a minimal, parseable BridgeConfig for the pipeline tests.
func testConfig(id string, version int, logLevel string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Version: version,
		Bridge: ports.BridgeSettings{
			ID:             id,
			DeploymentMode: "standalone",
			LogLevel:       logLevel,
		},
	}
}

// commit drives one in-band admin commit against the pipeline: it acts as both
// the httpapi caller (applyCommitted) and the Supervisor (draining the merged
// channel and reporting the swap outcome via onSwap). It returns the error
// applyCommitted produced.
func commit(t *testing.T, ctx context.Context, p *reloadPipeline, cfg *ports.BridgeConfig, swapErr error) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(ctx, cfg) }()

	// The Supervisor drains the merged channel and applies the config...
	got := receiveConfig(t, p.changes())
	if got != cfg {
		t.Fatalf("supervisor received a different config pointer than the applier fed in")
	}
	// ...then reports the swap outcome, which resolves the waiting applier.
	p.onSwap(bridge.SwapEvent{NewConfig: got, Error: swapErr})

	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not return after the swap was reported")
		return nil
	}
}

// TestReloadPipeline_SkipsWatcherReEmitOfInBandCommit is the double-rebuild
// regression: after an admin commit applies in-band, the file watcher re-emits
// the same config (the commit's durable write changed the file hash). The
// pipeline must recognise that re-emit as already-applied and DROP it, so the
// Supervisor swaps exactly once per commit — while a genuine external edit still
// flows through.
func TestReloadPipeline_SkipsWatcherReEmitOfInBandCommit(t *testing.T) {
	reg := ports.NewRegistry()
	p := newReloadPipeline(reg, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileCh := make(chan *ports.BridgeConfig, 2)
	go p.run(ctx, fileCh)

	// Admin commit applies in-band exactly once.
	adminCfg := testConfig("bridge-demo", 1, "debug")
	if err := commit(t, ctx, p, adminCfg, nil); err != nil {
		t.Fatalf("applyCommitted returned an error: %v", err)
	}

	// The watcher re-emits the committed config as it parses it back from disk:
	// Parse(MarshalYAML(cfg)). This must be skipped, so the following genuine
	// change (external edit) is the FIRST thing the supervisor sees next.
	reEmit, err := reparse(adminCfg, reg)
	if err != nil {
		t.Fatalf("reparse admin config: %v", err)
	}
	external := testConfig("bridge-demo", 0, "warn") // a real, different edit

	fileCh <- reEmit   // must be deduped
	fileCh <- external // must be forwarded

	got := receiveConfig(t, p.changes())
	if fingerprint(got) != fingerprint(external) {
		t.Fatalf("expected the redundant watcher re-emit to be skipped and the external edit forwarded; "+
			"got a config with fingerprint %s (external=%s, reEmit=%s)",
			fingerprint(got), fingerprint(external), fingerprint(reEmit))
	}
}

// TestReloadPipeline_ForwardsGenuineExternalEditWithoutCommit proves the dedup
// filter is inert without an in-band commit: file changes flow straight through
// (no fingerprint has been recorded yet).
func TestReloadPipeline_ForwardsGenuineExternalEditWithoutCommit(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileCh := make(chan *ports.BridgeConfig, 1)
	go p.run(ctx, fileCh)

	external := testConfig("bridge-demo", 3, "info")
	fileCh <- external

	got := receiveConfig(t, p.changes())
	if got != external {
		t.Fatal("a file change must be forwarded to the supervisor when no in-band commit precedes it")
	}
}

// TestReloadPipeline_CommittedNotAppliedOnSwapError is the committed_not_applied
// regression: when the Supervisor reports a failed swap, applyCommitted must
// return the (wrapped) error so httpapi surfaces committed_not_applied rather
// than a false "committed". A failed commit must NOT record a fingerprint, so a
// later watcher re-emit of that config is still retried.
func TestReloadPipeline_CommittedNotAppliedOnSwapError(t *testing.T) {
	reg := ports.NewRegistry()
	p := newReloadPipeline(reg, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileCh := make(chan *ports.BridgeConfig, 1)
	go p.run(ctx, fileCh)

	swapErr := errors.New("runtime build failed")
	adminCfg := testConfig("bridge-demo", 5, "debug")

	err := commit(t, ctx, p, adminCfg, swapErr)
	if err == nil {
		t.Fatal("applyCommitted must return an error when the swap fails (committed_not_applied)")
	}
	if !errors.Is(err, swapErr) {
		t.Fatalf("returned error must wrap the swap error; got %v", err)
	}

	// The failed apply recorded no fingerprint, so the watcher's re-emit of the
	// committed config is NOT skipped — it is retried.
	reEmit, rerr := reparse(adminCfg, reg)
	if rerr != nil {
		t.Fatalf("reparse admin config: %v", rerr)
	}
	fileCh <- reEmit
	got := receiveConfig(t, p.changes())
	if fingerprint(got) != fingerprint(reEmit) {
		t.Fatal("after a failed in-band apply, the watcher re-emit must be retried, not skipped")
	}
}

// TestReloadPipeline_ClearsFingerprintOnExternalEditThenRevert pins the
// stale-fingerprint regression: after an in-band commit records fp(B), an
// external file edit to A must invalidate it, so a later revert of the file
// back to B's content is FORWARDED (the runtime runs A while disk now holds B)
// rather than skipped as a redundant re-emit — which would strand the runtime
// on A permanently while disk says B.
func TestReloadPipeline_ClearsFingerprintOnExternalEditThenRevert(t *testing.T) {
	reg := ports.NewRegistry()
	p := newReloadPipeline(reg, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileCh := make(chan *ports.BridgeConfig, 1)
	go p.run(ctx, fileCh)

	// (1) Admin commit B applies in-band and records fp(B).
	configB := testConfig("bridge-demo", 1, "debug")
	if err := commit(t, ctx, p, configB, nil); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	// (2) Operator hand-edits the file to A; the watcher emits A. It differs
	// from B, so it must be forwarded — and forwarding it must clear fp(B).
	configA := testConfig("bridge-demo", 2, "warn")
	fileCh <- configA
	if got := receiveConfig(t, p.changes()); fingerprint(got) != fingerprint(configA) {
		t.Fatalf("external edit A must be forwarded; got fingerprint %s (A=%s)",
			fingerprint(got), fingerprint(configA))
	}

	// (3) Operator reverts the file back to B's content; the watcher re-emits B
	// as it parses from disk: Parse(MarshalYAML(B)). The runtime currently runs
	// A, so this genuine change MUST be forwarded, not skipped against a stale
	// fp(B). Without the clear-on-forward fix, receiveConfig here times out
	// because the revert is dropped as "redundant".
	revert, err := reparse(configB, reg)
	if err != nil {
		t.Fatalf("reparse B: %v", err)
	}
	fileCh <- revert
	if got := receiveConfig(t, p.changes()); fingerprint(got) != fingerprint(revert) {
		t.Fatalf("revert to B must be forwarded after an intervening external edit, not skipped as redundant; "+
			"got fingerprint %s (revert=%s)", fingerprint(got), fingerprint(revert))
	}
}

// TestReloadPipeline_ApplierUnblocksOnContextCancel proves applyCommitted does
// not hang if the Supervisor never drains the channel and the request context
// is cancelled.
func TestReloadPipeline_ApplierUnblocksOnContextCancel(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	// No supervisor drains p.changes(), so the merged channel backpressures.
	go p.run(runCtx, make(chan *ports.BridgeConfig))

	reqCtx, reqCancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(reqCtx, testConfig("bridge-demo", 7, "info")) }()

	reqCancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected a context-cancelled error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not unblock on context cancellation")
	}
}

// TestReloadPipeline_FileStreamCloseKeepsAdminPathAlive proves that a closed
// file-watcher stream does not tear down the pipeline: admin commits still apply.
func TestReloadPipeline_FileStreamCloseKeepsAdminPathAlive(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	close(fileCh) // watcher failure

	// The admin path must still converge.
	if err := commit(t, ctx, p, testConfig("bridge-demo", 9, "debug"), nil); err != nil {
		t.Fatalf("admin commit must still apply after the file stream closes: %v", err)
	}
}
