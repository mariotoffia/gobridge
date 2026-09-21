package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// contentTestTransport is the plugin kind the content-identity config below
// hangs its sessions, receivers, senders and bindings on. Its decoder carries no
// options, which is all the redundant-reload check needs: what is compared is
// the shape of the document, not what a transport does with it.
const contentTestTransport = "content-test"

// contentTestConfig builds a config whose id-keyed lists each carry two entries,
// so a test can reorder them and still describe the same bridge.
func contentTestConfig(version int, logLevel string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Version: version,
		Bridge: ports.BridgeSettings{
			ID:             "bridge-demo",
			DeploymentMode: "standalone",
			LogLevel:       logLevel,
		},
		Sessions: []ports.SessionDef{
			{ID: "session-a", Transport: contentTestTransport},
			{ID: "session-b", Transport: contentTestTransport},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "receiver-a", SessionID: "session-a"},
			{ID: "receiver-b", SessionID: "session-b"},
		},
		Senders: []ports.SenderDef{
			{ID: "sender-a", SessionID: "session-a"},
			{ID: "sender-b", SessionID: "session-b"},
		},
		Bindings: []ports.BindingDef{
			{ID: "binding-a", SenderID: "sender-a", Address: "queue-a"},
			{ID: "binding-b", SenderID: "sender-b", Address: "queue-b"},
		},
		Routes: []ports.RouteDef{
			{ID: "route-a", ReceiverID: "receiver-a", Bindings: []string{"binding-a"}},
			{ID: "route-b", ReceiverID: "receiver-b", Bindings: []string{"binding-b"}},
		},
	}
}

// reorderedContentTestConfig returns the same bridge with every id-keyed list
// written in the opposite order. Each of those lists is referred to by id
// everywhere else in the document, so the order it is written in carries no
// meaning.
func reorderedContentTestConfig(version int, logLevel string) *ports.BridgeConfig {
	cfg := contentTestConfig(version, logLevel)
	slices.Reverse(cfg.Sessions)
	slices.Reverse(cfg.Receivers)
	slices.Reverse(cfg.Senders)
	slices.Reverse(cfg.Bindings)
	slices.Reverse(cfg.Routes)
	return cfg
}

// TestReloadPipeline_RedundantReloadIsOnlyTheExactDocument pins what the skip is
// for: the watcher re-emits the exact document an in-band apply just wrote, and
// that single re-emit is what gets dropped. Every other document is forwarded —
// including one that says the same thing under a raised version or with its
// id-keyed lists written in another order. Whether such a document is a change
// is the Supervisor's question (it compares content — ADR 0016) and it adopts
// the new document when it is not, so the version the bridge reports follows
// the file instead of being stranded on the version the applier wrote.
func TestReloadPipeline_RedundantReloadIsOnlyTheExactDocument(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, reg.Register(contentTestTransport, func(ports.RawConfig) (ports.PluginConfig, error) {
		return nil, nil
	}))
	p := newReloadPipeline(reg, discardLogger())
	applied := contentTestConfig(1, "info")
	p.recordApplied(applied)

	reEmit, err := reparse(applied, reg)
	require.NoError(t, err)
	require.True(t, p.isRedundantFileReload(reEmit),
		"the watcher's re-emit of the applied document is the one reload that is skipped")

	for _, tc := range []struct {
		name     string
		reloaded *ports.BridgeConfig
	}{
		{"the same content under a raised version", contentTestConfig(2, "info")},
		{"the same content with its id-keyed lists reordered", reorderedContentTestConfig(1, "info")},
		{"a real edit", contentTestConfig(1, "debug")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, p.isRedundantFileReload(tc.reloaded),
				"a document other than the one just applied in-band must reach the Supervisor")
		})
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
// not treat acceptance by the pipeline as submission to the Supervisor. Until
// the Supervisor drains changes(), request cancellation is rollback-safe and
// must unblock the applier.
func TestReloadPipeline_ApplierUnblocksOnContextCancel(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	pipelineAccepted := make(chan struct{})
	go func() {
		apply := <-p.admin
		close(pipelineAccepted)
		p.forwardAdmin(context.Background(), apply)
	}()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(reqCtx, testConfig("bridge-demo", 7, "info")) }()

	<-pipelineAccepted
	reqCancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected a context-cancelled error, got %v", err)
		}
	case <-time.After(10 * time.Second):
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
