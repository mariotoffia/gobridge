package config

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches a configuration file for changes and re-parses it
// when modifications are detected. Rapid filesystem events are debounced.
type Watcher struct {
	path     string
	format   Format
	debounce time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

// WatcherOption configures a Watcher.
type WatcherOption func(*Watcher)

// WithDebounce sets the debounce interval for filesystem events.
func WithDebounce(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.debounce = d }
}

// WithFormat overrides format auto-detection.
func WithFormat(f Format) WatcherOption {
	return func(w *Watcher) { w.format = f }
}

// WithWatcherLogger sets the logger for watcher diagnostics.
func WithWatcherLogger(l *slog.Logger) WatcherOption {
	return func(w *Watcher) { w.logger = l }
}

// NewWatcher creates a Watcher for the given config file path.
func NewWatcher(path string, opts ...WatcherOption) *Watcher {
	w := &Watcher{
		path:     path,
		format:   FormatAuto,
		debounce: 100 * time.Millisecond,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Watch starts watching and emits parsed BridgeConfig values on the returned
// channel whenever the file changes. The channel is closed when ctx is
// cancelled or Stop is called. The initial config is NOT emitted; the
// caller should call ParseFile separately for the first load.
func (w *Watcher) Watch(ctx context.Context) (<-chan *BridgeConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return nil, errors.New("config: watcher already running")
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(w.path)
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, err
	}

	w.stopCh = make(chan struct{})
	w.running = true

	ch := make(chan *BridgeConfig, 1)
	go w.loop(ctx, fsw, ch)
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

func (w *Watcher) loop(ctx context.Context, fsw *fsnotify.Watcher, ch chan<- *BridgeConfig) {
	var debounceTimer *time.Timer
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
				debounceTimer = time.NewTimer(w.debounce)
				debounceCh = debounceTimer.C
			} else {
				debounceTimer.Reset(w.debounce)
			}

		case <-debounceCh:
			debounceTimer = nil
			debounceCh = nil

			cfg, err := ParseFile(w.path, w.format)
			if err != nil {
				if w.logger != nil {
					w.logger.Warn("config watcher: parse failed", "path", w.path, "error", err)
				}
				continue
			}
			select {
			case ch <- cfg:
			default:
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			if w.logger != nil {
				w.logger.Warn("config watcher: fsnotify error", "error", err)
			}
		}
	}
}
