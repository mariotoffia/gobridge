package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock"
)

const (
	defaultDebounce     = 100 * time.Millisecond
	defaultPollInterval = 30 * time.Second
)

// WatchMode selects the file change detection mechanism.
type WatchMode int

const (
	// ModeNotify uses fsnotify filesystem event notifications (default).
	ModeNotify WatchMode = iota
	// ModePoll uses periodic file reads with content hash comparison.
	ModePoll
)

// Watcher watches a configuration file for changes and re-parses it
// when modifications are detected. It supports two modes: fsnotify-based
// event watching (notify) and periodic content polling (poll).
type Watcher struct {
	path         string
	format       config.Format
	mode         WatchMode
	debounce     time.Duration
	pollInterval time.Duration
	logger       *slog.Logger
	clk          clock.Clock

	mu          sync.Mutex
	running     bool
	stopCh      chan struct{}
	started     chan struct{}
	startedOnce sync.Once
	lastApplied atomic.Pointer[time.Time]
}

// WatcherOption configures a Watcher.
type WatcherOption func(*Watcher)

// WithDebounce sets the debounce interval for filesystem events in
// notify mode.
func WithDebounce(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.debounce = d }
}

// WithPollInterval sets the polling interval for poll mode.
func WithPollInterval(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.pollInterval = d }
}

// WithMode sets the watch mode (ModeNotify or ModePoll).
func WithMode(m WatchMode) WatcherOption {
	return func(w *Watcher) { w.mode = m }
}

// WithFormat overrides format auto-detection for the watched file.
func WithFormat(f config.Format) WatcherOption {
	return func(w *Watcher) { w.format = f }
}

// WithLogger sets the logger for watcher diagnostics.
func WithLogger(l *slog.Logger) WatcherOption {
	return func(w *Watcher) { w.logger = l }
}

// WithClock overrides the clock used for timers and tickers.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) WatcherOption {
	return func(w *Watcher) { w.clk = c }
}

// WithWatchConfig applies settings from a ConfigWatchDef (typically read
// from the YAML config itself). Nil is safe; the call is a no-op.
func WithWatchConfig(def *config.ConfigWatchDef) WatcherOption {
	return func(w *Watcher) {
		if def == nil {
			return
		}
		switch def.Mode {
		case "poll":
			w.mode = ModePoll
		case "notify", "":
			w.mode = ModeNotify
		}
		if def.PollInterval != "" {
			if d, err := time.ParseDuration(def.PollInterval); err == nil && d > 0 {
				w.pollInterval = d
			}
		}
		if def.Debounce != "" {
			if d, err := time.ParseDuration(def.Debounce); err == nil && d > 0 {
				w.debounce = d
			}
		}
	}
}

// NewWatcher creates a file Watcher for the given config file path.
func NewWatcher(path string, opts ...WatcherOption) *Watcher {
	w := &Watcher{
		path:         path,
		format:       config.FormatAuto,
		mode:         ModeNotify,
		debounce:     defaultDebounce,
		pollInterval: defaultPollInterval,
		started:      make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	if w.clk == nil {
		w.clk = clock.System
	}
	return w
}

// Watch starts watching and emits parsed BridgeConfig values on the
// returned channel whenever the file changes. The channel is closed
// when ctx is cancelled or Stop is called. The initial config is NOT
// emitted; use Source.Load for the first load.
func (w *Watcher) Watch(ctx context.Context) (<-chan *config.BridgeConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return nil, errors.New("file config watcher: already running")
	}

	w.stopCh = make(chan struct{})
	w.running = true

	ch := make(chan *config.BridgeConfig, 1)

	switch w.mode {
	case ModePoll:
		w.startedOnce.Do(func() { close(w.started) })
		go w.pollLoop(ctx, ch)
	default:
		fsw, err := fsnotify.NewWatcher()
		if err != nil {
			w.running = false
			return nil, err
		}
		dir := filepath.Dir(w.path)
		if err := fsw.Add(dir); err != nil {
			_ = fsw.Close()
			w.running = false
			return nil, err
		}
		w.startedOnce.Do(func() { close(w.started) })
		go w.notifyLoop(ctx, fsw, ch)
	}

	return ch, nil
}

// Stop stops the watcher.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	close(w.stopCh)
	w.running = false
}

// Started returns a channel that is closed once the watcher's file monitoring
// is registered and ready to detect changes.
func (w *Watcher) Started() <-chan struct{} { return w.started }

// LastApplied returns the timestamp of the last successfully applied config
// change, or the zero value if no change has been applied yet.
func (w *Watcher) LastApplied() time.Time {
	if p := w.lastApplied.Load(); p != nil {
		return *p
	}
	return time.Time{}
}

// notifyLoop uses fsnotify for file change detection with debouncing.
func (w *Watcher) notifyLoop(ctx context.Context, fsw *fsnotify.Watcher, ch chan<- *config.BridgeConfig) {
	var debounceTimer clock.Timer
	var debounceCh <-chan time.Time

	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		_ = fsw.Close()
		close(ch)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return

		case event, ok := <-fsw.Events:
			if !ok {
				return
			}
			absPath, _ := filepath.Abs(w.path)
			eventPath, _ := filepath.Abs(event.Name)
			if absPath != eventPath {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if debounceTimer == nil {
				debounceTimer = w.clk.NewTimer(w.debounce)
				debounceCh = debounceTimer.C()
			} else {
				debounceTimer.Reset(w.debounce)
			}

		case <-debounceCh:
			debounceTimer = nil
			debounceCh = nil
			w.emitParsed(ch)

		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			if w.logger != nil {
				w.logger.Warn("file config watcher: fsnotify error", "error", err)
			}
		}
	}
}

// pollLoop periodically reads the file and emits on content change.
func (w *Watcher) pollLoop(ctx context.Context, ch chan<- *config.BridgeConfig) {
	defer func() {
		close(ch)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	lastHash := fileHash(w.path)

	ticker := w.clk.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C():
			h := fileHash(w.path)
			if h == lastHash {
				continue
			}
			lastHash = h
			w.emitParsed(ch)
		}
	}
}

func (w *Watcher) emitParsed(ch chan<- *config.BridgeConfig) {
	cfg, err := config.ParseFile(w.path, w.format)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("file config watcher: parse failed", "path", w.path, "error", err)
		}
		return
	}
	select {
	case ch <- cfg:
		now := w.clk.Now()
		w.lastApplied.Store(&now)
	default:
	}
}

func fileHash(path string) [sha256.Size]byte {
	f, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [sha256.Size]byte{}
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
