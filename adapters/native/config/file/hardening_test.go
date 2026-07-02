package file

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestEmitParsed_CoalescesLatestWins is the I4 regression: when the one-slot
// consumer channel is full, a newly parsed reload must supersede the queued
// one (latest-wins) instead of being silently dropped. Pre-fix the second
// reload hit the default branch and was discarded, leaving the consumer stuck
// on the stale config.
func TestEmitParsed_CoalescesLatestWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	w := NewWatcher(path, newTestRegistry(t))
	ch := make(chan *ports.BridgeConfig, 1)

	writeYAML(t, path, "A")
	w.emitParsed(ch) // queues A; slot now full and no consumer reads it

	writeYAML(t, path, "B")
	w.emitParsed(ch) // must coalesce to B, not drop it

	if got := w.CoalescedReloads(); got != 1 {
		t.Fatalf("CoalescedReloads = %d, want 1", got)
	}

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "B" {
			t.Fatalf("consumer read stale config %q, want latest %q", cfg.Bridge.ID, "B")
		}
	default:
		t.Fatal("expected the latest config to be queued")
	}
}

// TestEmitParsed_NoCoalesceWhenConsumerKeepsUp verifies the fast path does not
// falsely count coalescing when the consumer drains between reloads.
func TestEmitParsed_NoCoalesceWhenConsumerKeepsUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	w := NewWatcher(path, newTestRegistry(t))
	ch := make(chan *ports.BridgeConfig, 1)

	writeYAML(t, path, "A")
	w.emitParsed(ch)
	if cfg := <-ch; cfg.Bridge.ID != "A" {
		t.Fatalf("got %q, want A", cfg.Bridge.ID)
	}

	writeYAML(t, path, "B")
	w.emitParsed(ch)
	if cfg := <-ch; cfg.Bridge.ID != "B" {
		t.Fatalf("got %q, want B", cfg.Bridge.ID)
	}

	if got := w.CoalescedReloads(); got != 0 {
		t.Fatalf("CoalescedReloads = %d, want 0", got)
	}
}

// TestSource_Load_RespectsContextCancellation is the I6 regression for the
// config source: a cancelled context short-circuits before parsing.
func TestSource_Load_RespectsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "x")

	src := NewSource(path, newTestRegistry(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := src.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestSource_Load_MissingFileMapsNotFound verifies a missing config file
// surfaces as the classified shared.ErrNotFound (I6 error mapping).
func TestSource_Load_MissingFileMapsNotFound(t *testing.T) {
	src := NewSource(filepath.Join(t.TempDir(), "does-not-exist.yaml"), newTestRegistry(t))

	_, err := src.Load(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected shared.ErrNotFound, got %v", err)
	}
}
