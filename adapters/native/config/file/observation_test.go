package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestObserveFileAbsenceFaultAndRecreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	fc := clocktest.New()
	w := NewWatcher(path, newTestRegistry(t), WithMode(ModePoll), WithClock(fc))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := w.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	var sequence uint64
	expect := func(kind ports.ConfigObservationKind) {
		t.Helper()
		got := wait.RequireReceive(t, out, time.Second)
		if got.Kind != kind || got.Sequence <= sequence {
			t.Fatalf("observation: %+v after sequence %d", got, sequence)
		}
		sequence = got.Sequence
	}
	expect(ports.ConfigMissing)
	waitForTicker(t, fc)
	if _, err := w.Watch(ctx); err == nil {
		t.Fatal("Watch must not start a second poller")
	}
	present := func() {
		t.Helper()
		writeYAML(t, path, "same")
		fc.Advance(w.pollInterval)
		waitForTimer(t, fc)
		fc.Advance(w.debounce)
		expect(ports.ConfigPresent)
	}
	present()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fc.Advance(w.pollInterval)
	expect(ports.ConfigMissing)
	present() // identical bytes and reset version are still a new presence.
	if err := os.WriteFile(path, []byte("["), 0o600); err != nil {
		t.Fatal(err)
	}
	fc.Advance(w.pollInterval)
	waitForTimer(t, fc)
	fc.Advance(w.debounce)
	expect(ports.ConfigReadError)
	present()
	cancel()
	w.Stop()
	wait.RequireClosed(t, out, time.Second)
}

func TestObserveFileInitialSnapshotAfterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeYAML(t, path, "old")
	source := NewSource(path, newTestRegistry(t))
	if _, err := source.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	hash, _ := source.LoadHash()
	writeYAML(t, path, "new")
	w := NewWatcher(path, newTestRegistry(t), WithBaselineHash(hash), WithMode(ModePoll))
	out, err := w.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	got := wait.RequireReceive(t, out, time.Second)
	if got.Kind != ports.ConfigPresent || got.Config.Bridge.ID != "new" {
		t.Fatalf("initial snapshot: %+v", got)
	}
}

func TestObserveFileParserNotFoundIsReadFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("bridge: {id: invalid}\nsessions: [{id: s, transport: bad}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := newTestRegistry(t)
	if err := reg.Register("bad", func(ports.RawConfig) (ports.PluginConfig, error) {
		return nil, shared.ErrNotFound
	}); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(path, reg, WithMode(ModePoll))
	out, err := w.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if got := wait.RequireReceive(t, out, time.Second); got.Kind != ports.ConfigReadError {
		t.Fatalf("existing invalid document misclassified: %+v", got)
	}
}

func TestObserveFileRestartKeepsSequence(t *testing.T) {
	w := NewWatcher(filepath.Join(t.TempDir(), "missing.yaml"), newTestRegistry(t), WithMode(ModePoll))
	var sequence uint64
	for range 2 {
		out, err := w.Observe(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		got := wait.RequireReceive(t, out, time.Second)
		if got.Sequence <= sequence {
			t.Fatalf("sequence regressed: %d after %d", got.Sequence, sequence)
		}
		sequence = got.Sequence
		w.Stop()
		wait.RequireClosed(t, out, time.Second)
	}
}
