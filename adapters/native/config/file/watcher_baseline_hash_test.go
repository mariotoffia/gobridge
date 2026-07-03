package file

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// TestWatcher_WithBaselineHash_EmitsChangeWrittenBetweenLoadAndWatch is the
// regression for the baseline-hash window: a change written AFTER the caller's
// Source.Load but BEFORE Watch must still be detected. Previously Watch
// re-hashed disk at start, so a between-the-two edit was absorbed into the
// baseline and never emitted — the runtime silently ran a stale config until
// the next edit. Baselining the watcher from Source.LoadHash closes the window.
func TestWatcher_WithBaselineHash_EmitsChangeWrittenBetweenLoadAndWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "v1")

	reg := newTestRegistry(t)
	src := NewSource(path, reg)

	cfg, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bridge.ID != "v1" {
		t.Fatalf("expected v1, got %q", cfg.Bridge.ID)
	}
	baseline, ok := src.LoadHash()
	if !ok {
		t.Fatal("LoadHash must report a hash after a successful Load")
	}

	// The file changes AFTER Load but BEFORE Watch — the window the fix closes.
	writeYAML(t, path, "v2")

	fc := clocktest.New()
	w := NewWatcher(path, reg,
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
		WithClock(fc),
		WithBaselineHash(baseline),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	waitForTicker(t, fc)
	fc.Advance(100 * time.Millisecond)

	select {
	case got := <-ch:
		if got.Bridge.ID != "v2" {
			t.Fatalf("expected the between-Load-and-Watch change v2 to be emitted, got %q", got.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: change written between Load and Watch was never emitted")
	}
}

// TestWatcher_WithBaselineHash_IdenticalRewriteEmitsNothing verifies the fix
// does not over-fire: when the file is unchanged relative to the loaded content,
// baselining from Source.LoadHash still dedups a byte-identical rewrite.
func TestWatcher_WithBaselineHash_IdenticalRewriteEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "same")

	reg := newTestRegistry(t)
	src := NewSource(path, reg)
	if _, err := src.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	baseline, ok := src.LoadHash()
	if !ok {
		t.Fatal("LoadHash must report a hash after a successful Load")
	}

	fc := clocktest.New()
	w := NewWatcher(path, reg,
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
		WithClock(fc),
		WithBaselineHash(baseline),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	waitForTicker(t, fc)
	// Byte-identical rewrite — content hash matches the baseline exactly.
	writeYAML(t, path, "same")
	fc.Advance(100 * time.Millisecond)

	select {
	case got := <-ch:
		t.Fatalf("byte-identical rewrite must not emit a config, got %q", got.Bridge.ID)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing emitted
	}
}
