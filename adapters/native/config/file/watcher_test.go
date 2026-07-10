package file

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

type stubPluginConfig struct{ kind string }

func (s stubPluginConfig) Kind() string    { return s.kind }
func (s stubPluginConfig) Validate() error { return nil }

// newTestRegistry returns a hermetic *ports.Registry pre-populated
// with stub decoders for the transport kinds the test fixtures use.
// Each test constructs its own registry and passes it explicitly to
// NewSource / NewWatcher; there is no process-wide singleton.
func newTestRegistry(t testing.TB) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	for _, k := range []string{"mqtt", "sqs", "http"} {
		kind := k
		if err := reg.Register(kind, func(raw ports.RawConfig) (ports.PluginConfig, error) {
			return stubPluginConfig{kind: kind}, nil
		}); err != nil {
			t.Fatalf("register stub %q: %v", kind, err)
		}
	}
	return reg
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

// readRecorder wraps a Watcher.readFile seam to count invocations and capture
// the bytes returned by each read. It lets a test PROVE the HIGH-2 stability
// gate's confirm step performs an ACTUAL second read of identical bytes before
// emitting — rather than replaying cached first-sighting bytes when the confirm
// timer fires. Without this a regression that emitted the candidate on the timer
// without re-reading would still pass the loop tests. Safe for the watcher
// goroutine to call while the test goroutine inspects it.
type readRecorder struct {
	inner func(string) ([]byte, error)

	mu    sync.Mutex
	reads [][]byte
}

func newReadRecorder(inner func(string) ([]byte, error)) *readRecorder {
	return &readRecorder{inner: inner}
}

// read is the seam installed as Watcher.readFile.
func (r *readRecorder) read(p string) ([]byte, error) {
	data, err := r.inner(p)
	r.mu.Lock()
	r.reads = append(r.reads, append([]byte(nil), data...))
	r.mu.Unlock()
	return data, err
}

func (r *readRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reads)
}

// last returns a copy of the bytes returned by the most recent read.
func (r *readRecorder) last() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reads) == 0 {
		return nil
	}
	return r.reads[len(r.reads)-1]
}

func TestSource_Load(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test-bridge")

	src := NewSource(path, newTestRegistry(t))
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

	w := NewWatcher(path, newTestRegistry(t), WithDebounce(50*time.Millisecond))

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
	w := NewWatcher(path, newTestRegistry(t),
		WithMode(ModePoll),
		WithPollInterval(100*time.Millisecond),
		// Confirm window (debounce) STRICTLY smaller than the poll interval so
		// advancing through it fires ONLY the one-shot confirm timer, never a
		// second poll tick. That isolates the confirm re-read: the read count can
		// only grow if the CONFIRM path itself re-reads (a coincident ticker
		// re-read would otherwise mask a cache-emit regression).
		WithDebounce(20*time.Millisecond),
		WithClock(fc),
	)
	// Count reads through the seam so we can prove the confirm step RE-READS
	// the file (HIGH-2), not just replays cached first-sighting bytes.
	rec := newReadRecorder(os.ReadFile)
	w.readFile = rec.read

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	waitForTicker(t, fc)
	writeYAML(t, path, "polled")
	fc.Advance(100 * time.Millisecond) // poll tick detects the change → held for stability

	// Stability gate (HIGH-2): a detected change is applied only after a
	// confirming re-read one settle window (debounce) later returns the same
	// bytes. Poll mode arms a one-shot confirm timer for that re-read; advance
	// through it so the settled content is delivered.
	waitForTimer(t, fc)
	// The detect read set the candidate and armed the confirm timer but has NOT
	// emitted yet. Snapshot the read state so the confirm phase can be asserted
	// to perform a genuine second read.
	detectRead := rec.last()
	readsBeforeConfirm := rec.count()
	fc.Advance(w.debounce)

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "polled" {
			t.Fatalf("expected bridge id %q, got %q", "polled", cfg.Bridge.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for poll event")
	}

	// #4: the confirm must have re-read the file (not emitted cached bytes on the
	// timer), and that second read must have returned identical bytes.
	if got := rec.count(); got <= readsBeforeConfirm {
		t.Fatalf("confirm did not re-read the file: read count stayed at %d (expected > %d)", got, readsBeforeConfirm)
	}
	if confirmRead := rec.last(); !bytes.Equal(confirmRead, detectRead) {
		t.Fatalf("confirm re-read %q differs from the detect read %q; the gate must confirm on IDENTICAL bytes",
			confirmRead, detectRead)
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
	w := NewWatcher(path, newTestRegistry(t), WithWatchConfig(def))

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
	w := NewWatcher("/tmp/fake.yaml", newTestRegistry(t), WithWatchConfig(def))

	if w.mode != ModeNotify {
		t.Fatal("expected notify mode")
	}
	if w.debounce != 200*time.Millisecond {
		t.Fatalf("expected 200ms debounce, got %v", w.debounce)
	}
}

func TestWatcher_WithWatchConfig_Nil(t *testing.T) {
	w := NewWatcher("/tmp/fake.yaml", newTestRegistry(t), WithWatchConfig(nil))
	if w.mode != ModeNotify {
		t.Fatal("nil ConfigWatchDef should default to notify")
	}
}

func TestWatcher_AlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test")

	w := NewWatcher(path, newTestRegistry(t))

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

	w := NewWatcher(path, newTestRegistry(t), WithDebounce(200*time.Millisecond))

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
	w := NewWatcher(path, newTestRegistry(t),
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
	fc.Advance(100 * time.Millisecond) // poll tick detects the change

	// Advance through the stability window so the confirming read actually
	// reaches the parser: corrupt-but-stable content must still emit nothing
	// (and must not advance lastHash), exercising the parse-failure path.
	waitForTimer(t, fc)
	fc.Advance(w.debounce)

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
	w := NewWatcher("/tmp/fake.yaml", newTestRegistry(t), WithFormat(parser.FormatJSON))
	if w.format != parser.FormatJSON {
		t.Fatalf("expected FormatJSON, got %v", w.format)
	}
}

func TestWatcher_StopClosesChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "test")

	w := NewWatcher(path, newTestRegistry(t), WithMode(ModePoll), WithPollInterval(50*time.Millisecond))

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
