package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
)

func writeYAML(t *testing.T, path string, bridgeID string) {
	t.Helper()
	content := "bridge:\n  id: " + bridgeID + "\n" +
		"receivers:\n  - id: r1\n    transport: mqtt\n" +
		"senders:\n  - id: s1\n    transport: mqtt\n" +
		"bindings:\n  - id: b1\n    sender_id: s1\n    address: topic/out\n" +
		"routes:\n  - id: route1\n    receiver_id: r1\n    bindings: [b1]\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSource_Load(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test-bridge")

	src := NewSource(path)
	cfg, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bridge.ID != "test-bridge" {
		t.Fatalf("expected bridge id %q, got %q", "test-bridge", cfg.Bridge.ID)
	}
}

func TestWatcher_NotifyMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	w := NewWatcher(path, WithDebounce(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	writeYAML(t, path, "updated")

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "updated" {
			t.Fatalf("expected bridge id %q, got %q", "updated", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for notify event")
	}

	w.Stop()
}

func TestWatcher_PollMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	w := NewWatcher(path,
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	writeYAML(t, path, "polled")

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "polled" {
			t.Fatalf("expected bridge id %q, got %q", "polled", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for poll event")
	}

	w.Stop()
}

func TestWatcher_WithWatchConfig_Poll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	def := &config.ConfigWatchDef{
		Mode:         "poll",
		PollInterval: "100ms",
	}
	w := NewWatcher(path, WithWatchConfig(def))

	if w.mode != ModePoll {
		t.Fatal("expected poll mode")
	}
	if w.pollInterval != 100*time.Millisecond {
		t.Fatalf("expected 100ms poll interval, got %v", w.pollInterval)
	}
}

func TestWatcher_WithWatchConfig_Notify(t *testing.T) {
	def := &config.ConfigWatchDef{
		Mode:     "notify",
		Debounce: "200ms",
	}
	w := NewWatcher("/tmp/fake.yaml", WithWatchConfig(def))

	if w.mode != ModeNotify {
		t.Fatal("expected notify mode")
	}
	if w.debounce != 200*time.Millisecond {
		t.Fatalf("expected 200ms debounce, got %v", w.debounce)
	}
}

func TestWatcher_WithWatchConfig_Nil(t *testing.T) {
	w := NewWatcher("/tmp/fake.yaml", WithWatchConfig(nil))
	if w.mode != ModeNotify {
		t.Fatal("nil ConfigWatchDef should default to notify")
	}
}

func TestWatcher_AlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test")

	w := NewWatcher(path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.Watch(ctx)
	if err == nil {
		t.Fatal("second Watch should fail")
	}

	w.Stop()
}

func TestWatcher_StopClosesChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test")

	w := NewWatcher(path, WithMode(ModePoll), WithPollInterval(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	w.Stop()

	select {
	case _, ok := <-ch:
		if ok {
			// got a config before close, that's fine, drain
			<-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel should be closed after Stop")
	}
}
