package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

type stubPluginConfig struct{ kind string }

func (s stubPluginConfig) Kind() string    { return s.kind }
func (s stubPluginConfig) Validate() error { return nil }

func init() {
	for _, k := range []string{"mqtt", "sqs", "http"} {
		kind := k
		func() {
			defer func() { _ = recover() }()
			ports.DefaultRegistry.Register(kind, func(raw ports.RawConfig) (ports.PluginConfig, error) {
				return stubPluginConfig{kind: kind}, nil
			})
		}()
	}
}

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

	select {
	case <-w.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not start")
	}
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

	fc := clocktest.New()
	w := NewWatcher(path,
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
		WithClock(fc),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	waitForTicker(t, fc)
	writeYAML(t, path, "polled")
	fc.Advance(100 * time.Millisecond)

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

	def := &ports.ConfigWatchDef{
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
	def := &ports.ConfigWatchDef{
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

// TestWatcher_DebounceCoalesces validates that multiple rapid writes produce
// a single config event rather than one per write.
func TestWatcher_DebounceCoalesces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	w := NewWatcher(path, WithDebounce(200*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-w.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not start")
	}

	for i := 0; i < 5; i++ {
		writeYAML(t, path, "rapid-"+string(rune('0'+i)))
		// SYNC: space writes so fsnotify delivers separate events.
		time.Sleep(20 * time.Millisecond)
	}

	var received int
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break loop
			}
			received++
		case <-timeout:
			break loop
		}
	}

	if received < 1 {
		t.Fatal("expected at least one coalesced event")
	}
	if received > 2 {
		t.Fatalf("expected debounce to coalesce writes, got %d events from 5 writes", received)
	}

	w.Stop()
}

// TestWatcher_InvalidContent validates that corrupt YAML is handled gracefully
// (no config emitted, no crash, error logged).
func TestWatcher_InvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "valid")

	fc := clocktest.New()
	w := NewWatcher(path,
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
		WithClock(fc),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	waitForTicker(t, fc)
	if err := os.WriteFile(path, []byte("{{broken yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	fc.Advance(100 * time.Millisecond)

	select {
	case cfg := <-ch:
		t.Fatalf("should not emit config for corrupt content, got: %+v", cfg)
	case <-time.After(500 * time.Millisecond):
		// Expected: no event emitted for invalid content.
	}

	w.Stop()
}

// TestWatcher_WithFormat validates that WithFormat overrides the auto-detected
// format so a .yaml file can be parsed as JSON when explicitly told to.
func TestWatcher_WithFormat(t *testing.T) {
	w := NewWatcher("/tmp/fake.yaml", WithFormat(config.FormatJSON))
	if w.format != config.FormatJSON {
		t.Fatalf("expected FormatJSON, got %v", w.format)
	}
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

// waitForTicker spins until the fake clock has at least one active ticker,
// meaning the poll goroutine has started and registered its ticker.
func waitForTicker(t *testing.T, fc *clocktest.Fake) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fc.TickerCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for poll ticker to register")
		}
		time.Sleep(1 * time.Millisecond)
	}
}
