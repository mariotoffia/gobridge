package file

import (
	"bytes"
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
// hash-reconciliation backup: when a config change produces NO fsnotify event
// at all — a symlinked ConfigMap target written outside the watched directory,
// or a kernel queue drop — the periodic resync tick must still detect and
// deliver it.
//
// The missed event is modelled deterministically by driving runNotify with an
// injected event channel that stays empty (rather than a real fsnotify watcher
// on a symlink, whose event delivery is platform-dependent and races the fake
// clock). Only the resync ticker can notice the change; the stability gate then
// holds it for one settle window before applying.
func TestWatcher_Notify_ResyncCatchesMissedEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	fc := clocktest.New()
	w := NewWatcher(path, newTestRegistry(t), WithClock(fc))
	// Count reads through the seam so the confirm step can be proven to RE-READ.
	rec := newReadRecorder(os.ReadFile)
	w.readFile = rec.read
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
		<-doneCh
	})

	// Baseline as Watch would take it, then mutate the file with NO event
	// delivered — the injected event channel never carries one.
	var err error
	w.lastHash, err = fileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	writeYAML(t, path, "resynced")

	events := make(chan fsnotify.Event)
	errs := make(chan error)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *ports.BridgeConfig, 1)
	go w.runNotify(ctx, events, errs, func() error { return nil }, ch, stopCh, doneCh)

	waitForTicker(t, fc)         // resync ticker registered
	fc.Advance(w.resyncInterval) // resync tick: detects the missed change, holds it pending

	// Stability gate (HIGH-2): the resync-detected change is applied only after
	// a confirming re-read one settle window (debounce) later returns the same
	// bytes. The resync path arms that debounce re-read.
	waitForTimer(t, fc)
	detectRead := rec.last()
	readsBeforeConfirm := rec.count()
	fc.Advance(w.debounce)

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "resynced" {
			t.Fatalf("expected bridge id %q, got %q", "resynced", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: resync tick did not deliver the missed change")
	}

	// #4: the confirm must have re-read the file (a genuine second read of
	// identical bytes), not replayed cached first-sighting bytes on the timer.
	if got := rec.count(); got <= readsBeforeConfirm {
		t.Fatalf("confirm did not re-read the file: read count stayed at %d (expected > %d)", got, readsBeforeConfirm)
	}
	if confirmRead := rec.last(); !bytes.Equal(confirmRead, detectRead) {
		t.Fatalf("confirm re-read %q differs from the detect read %q; the gate must confirm on IDENTICAL bytes",
			confirmRead, detectRead)
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
	fc.Advance(defaultDebounce) // debounce fires: change detected, held for stability

	// Stability gate (HIGH-2): the confirming re-read happens one settle window
	// later (the debounce path re-arms itself when a change is pending). Advance
	// through it so the settled change is delivered.
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

	// The overflow forces a hash-check reload; the stability gate holds the
	// detected change until a confirming re-read one settle window (debounce)
	// later returns the same bytes. The errs path arms that debounce re-read.
	waitForTimer(t, fc)
	fc.Advance(w.debounce)

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
