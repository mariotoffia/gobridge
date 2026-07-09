package file

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// countingHandler counts WARN-and-above records so a test can assert how many
// times the watcher actually logged, without parsing output.
type countingHandler struct {
	warns atomic.Int64
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.warns.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// TestReloadIfChanged_ReadFailureLoggedOncePerStreak proves the read-failure
// WARN is de-duplicated to one per fail streak. A start-empty bridge watches a
// file that does not exist yet, and notify mode matches events per-directory —
// without the dedup every unrelated event in a busy directory re-logs the same
// missing file (observed: ~10 WARNs/second against /tmp).
func TestReloadIfChanged_ReadFailureLoggedOncePerStreak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.yaml")
	h := &countingHandler{}
	w := NewWatcher(path, newTestRegistry(t), WithLogger(slog.New(h)))
	ch := make(chan *ports.BridgeConfig, 1)

	// Fail streak: many reload attempts against a missing file log once.
	for range 5 {
		w.reloadIfChanged(ch)
	}
	if got := h.warns.Load(); got != 1 {
		t.Fatalf("a fail streak must log exactly one WARN, got %d", got)
	}

	// A successful read ends the streak...
	if err := os.WriteFile(path, []byte("bridge:\n  id: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reloadIfChanged(ch)
	if got := h.warns.Load(); got != 1 {
		t.Fatalf("a successful read must not warn, got %d", got)
	}

	// ...so the next failure is a new streak and logs again (exactly once).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		w.reloadIfChanged(ch)
	}
	if got := h.warns.Load(); got != 2 {
		t.Fatalf("a new fail streak must log exactly one more WARN, got %d total", got)
	}
}
