package file

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// TestWatcher_Notify_ConfigMapSymlinkSwap reproduces the Kubernetes
// ConfigMap update mechanism: the config path resolves through a
// "..data" symlink that is atomically retargeted on update, so the
// watched file path itself never receives a Write event. The pre-fix
// exact-path event filter missed every such update forever, and no
// reconciliation backed fsnotify up. Post-fix the swap must be applied
// either via the directory-scoped event filter or — when the platform
// (e.g. kqueue) drops the event entirely — via the hash-resync backstop
// within one resync interval.
func TestWatcher_Notify_ConfigMapSymlinkSwap(t *testing.T) {
	dir := t.TempDir()

	v1 := filepath.Join(dir, "..data_v1")
	v2 := filepath.Join(dir, "..data_v2")
	for _, d := range []string{v1, v2} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeYAML(t, filepath.Join(v1, "bridge.yaml"), "initial")
	writeYAML(t, filepath.Join(v2, "bridge.yaml"), "swapped")

	data := filepath.Join(dir, "..data")
	if err := os.Symlink(v1, data); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.Symlink(filepath.Join(data, "bridge.yaml"), cfgPath); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(cfgPath, newTestRegistry(t),
		WithDebounce(50*time.Millisecond),
		WithResyncInterval(200*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	select {
	case <-w.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not start")
	}

	// Kubernetes-style atomic swap: new symlink, rename over ..data.
	// Only "..data_tmp" / "..data" events fire — never the config path.
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(v2, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, data); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "swapped" {
			t.Fatalf("expected bridge id %q, got %q", "swapped", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: ConfigMap-style symlink swap was not detected")
	}
}

// TestWatcher_Notify_ResyncCatchesMissedEvents proves the notify-mode
// hash-reconciliation backup: the config path is a symlink whose target
// lives OUTSIDE the watched directory, so writing the target produces
// no fsnotify event at all. The periodic resync tick must still detect
// and deliver the change within one interval.
func TestWatcher_Notify_ResyncCatchesMissedEvents(t *testing.T) {
	watchDir := t.TempDir()
	targetDir := t.TempDir()

	target := filepath.Join(targetDir, "bridge.yaml")
	writeYAML(t, target, "initial")
	cfgPath := filepath.Join(watchDir, "bridge.yaml")
	if err := os.Symlink(target, cfgPath); err != nil {
		t.Fatal(err)
	}

	fc := clocktest.New()
	w := NewWatcher(cfgPath, newTestRegistry(t), WithClock(fc))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	waitForTicker(t, fc) // resync ticker registered → baseline hash taken

	writeYAML(t, target, "resynced") // no event in watchDir
	fc.Advance(defaultResyncInterval)

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "resynced" {
			t.Fatalf("expected bridge id %q, got %q", "resynced", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: resync tick did not deliver the missed change")
	}
}

// TestWatcher_Notify_DedupIdenticalRewrite verifies content dedup: a
// byte-identical rewrite (ArgoCD/Ansible re-sync) fires fsnotify events
// but must NOT deliver a config — a stop-the-world runtime swap for an
// unchanged file is pure cost. A subsequent real change must still be
// delivered.
func TestWatcher_Notify_DedupIdenticalRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "same")

	fc := clocktest.New()
	w := NewWatcher(path, newTestRegistry(t), WithClock(fc))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	waitForTicker(t, fc) // loop running, baseline hash taken

	// Identical rewrite: event fires, debounce arms, hash gate suppresses.
	writeYAML(t, path, "same")
	waitForTimer(t, fc)
	fc.Advance(defaultDebounce)

	select {
	case cfg := <-ch:
		t.Fatalf("identical rewrite must not deliver a config, got %q", cfg.Bridge.ID)
	case <-time.After(200 * time.Millisecond):
		// expected: deduped
	}

	// Real change still delivered.
	writeYAML(t, path, "changed")
	waitForTimer(t, fc)
	fc.Advance(defaultDebounce)

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "changed" {
			t.Fatalf("expected bridge id %q, got %q", "changed", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for changed content after dedup")
	}
}

// TestWatcher_Notify_OverflowForcesResync verifies that an fsnotify
// error — most importantly fsnotify.ErrEventOverflow, meaning the
// kernel dropped events — forces an immediate hash-check reload rather
// than only logging and waiting for luck.
func TestWatcher_Notify_OverflowForcesResync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	fc := clocktest.New()
	w := NewWatcher(path, newTestRegistry(t), WithClock(fc))
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
		<-doneCh
	})

	// Baseline as Watch would take it, then mutate the file with no
	// event delivered — the injected channels never carry one.
	var err error
	w.lastHash, err = fileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	writeYAML(t, path, "overflowed")

	events := make(chan fsnotify.Event)
	errs := make(chan error)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *ports.BridgeConfig, 1)
	go w.runNotify(ctx, events, errs, func() error { return nil }, ch, stopCh, doneCh)

	errs <- fsnotify.ErrEventOverflow

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "overflowed" {
			t.Fatalf("expected bridge id %q, got %q", "overflowed", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: overflow error did not force a resync reload")
	}
}

// waitForTimer spins until the fake clock has at least one armed timer
// (the notify loop's debounce), meaning the fsnotify event for the
// preceding write has been observed.
func waitForTimer(t *testing.T, fc *clocktest.Fake) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fc.TimerCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for debounce timer to arm")
		}
		runtime.Gosched()
	}
}
