package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	defaultDebounce     = 100 * time.Millisecond
	defaultPollInterval = 30 * time.Second
	// defaultResyncInterval is the notify-mode reconciliation cadence: a
	// slow content-hash poll that backs up fsnotify so a missed or
	// unmatchable filesystem event (Kubernetes ConfigMap ..data symlink
	// swaps, kernel event-queue overflow) is caught within one interval.
	defaultResyncInterval = 30 * time.Second
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
	path           string
	format         parser.Format
	registry       *ports.Registry
	mode           WatchMode
	debounce       time.Duration
	pollInterval   time.Duration
	resyncInterval time.Duration
	logger         *slog.Logger
	clk            clock.Clock

	mu          sync.Mutex
	running     bool
	stopCh      chan struct{}
	started     chan struct{}
	startedOnce sync.Once
	lastApplied atomic.Pointer[time.Time]

	// lastHash is the content hash of the last successfully parsed and
	// delivered config. It gates every reload (notify events, resync
	// ticks, poll ticks) so byte-identical rewrites — ArgoCD/Ansible
	// re-syncing an unchanged file — never trigger a runtime swap. Only
	// the single watch-loop goroutine (notify OR poll, never both)
	// touches it, so no lock is needed.
	lastHash [sha256.Size]byte

	// baselineHash, when baselineHashSet, seeds lastHash at Watch time instead
	// of hashing disk. Set via WithBaselineHash from the hash the caller
	// actually Loaded (Source.LoadHash), closing the Load↔Watch window where a
	// between-the-two edit would be absorbed into the baseline and never
	// emitted.
	baselineHash    [sha256.Size]byte
	baselineHashSet bool

	// coalescedReloads counts reloads whose predecessor was still queued and
	// had to be evicted so the newest config could be enqueued (I4). It is a
	// convergence signal, not a loss signal: the consumer still receives the
	// latest file state.
	coalescedReloads atomic.Uint64
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

// WithResyncInterval sets the notify-mode reconciliation interval: a
// periodic content-hash comparison that catches file changes fsnotify
// missed (Kubernetes ConfigMap symlink swaps, event-queue overflow).
// Non-positive values are ignored; the default is 30s.
func WithResyncInterval(d time.Duration) WatcherOption {
	return func(w *Watcher) {
		if d > 0 {
			w.resyncInterval = d
		}
	}
}

// WithMode sets the watch mode (ModeNotify or ModePoll).
func WithMode(m WatchMode) WatcherOption {
	return func(w *Watcher) { w.mode = m }
}

// WithFormat overrides format auto-detection for the watched file.
func WithFormat(f parser.Format) WatcherOption {
	return func(w *Watcher) { w.format = f }
}

// WithLogger sets the logger for watcher diagnostics.
func WithLogger(l *slog.Logger) WatcherOption {
	return func(w *Watcher) { w.logger = l }
}

// WithClock overrides the clock used for timers and tickers.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) WatcherOption {
	return func(w *Watcher) {
		if c != nil {
			w.clk = c
		}
	}
}

// WithBaselineHash seeds the watcher's change-detection baseline with a content
// hash the caller already observed (typically Source.LoadHash), instead of
// hashing the file at Watch time. This closes the Load↔Watch race: a change
// written between the caller's initial Load and this Watch is then detected and
// emitted, rather than silently folded into the baseline so the runtime keeps
// running a stale config until the next edit.
func WithBaselineHash(h [sha256.Size]byte) WatcherOption {
	return func(w *Watcher) {
		w.baselineHash = h
		w.baselineHashSet = true
	}
}

// WithWatchConfig applies settings from a ConfigWatchDef (typically read
// from the YAML config itself). Nil is safe; the call is a no-op.
func WithWatchConfig(def *ports.ConfigWatchDef) WatcherOption {
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
				// In notify mode PollInterval doubles as the resync
				// (hash-reconciliation) cadence so operators have a
				// single knob for "how stale can a missed event be".
				w.resyncInterval = d
			}
		}
		if def.Debounce != "" {
			if d, err := time.ParseDuration(def.Debounce); err == nil && d > 0 {
				w.debounce = d
			}
		}
	}
}

// NewWatcher creates a file Watcher for the given config file path
// and plugin registry. registry MUST be non-nil — it carries the
// PluginConfig decoders the two-stage parser needs.
func NewWatcher(path string, registry *ports.Registry, opts ...WatcherOption) *Watcher {
	w := &Watcher{
		path:           path,
		format:         parser.FormatAuto,
		registry:       registry,
		mode:           ModeNotify,
		debounce:       defaultDebounce,
		pollInterval:   defaultPollInterval,
		resyncInterval: defaultResyncInterval,
		started:        make(chan struct{}),
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
func (w *Watcher) Watch(ctx context.Context) (<-chan *ports.BridgeConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return nil, errors.New("file config watcher: already running")
	}

	w.stopCh = make(chan struct{})
	w.running = true

	ch := make(chan *ports.BridgeConfig, 1)

	// Baseline: content as of Watch start, taken synchronously so no
	// event/tick observed after Watch returns can race it. The initial
	// config is the caller's Load; the watcher only reports changes.
	//
	// When the caller supplied WithBaselineHash (the hash of the content it
	// actually Loaded), use that rather than re-hashing disk here — otherwise a
	// change written between Load and Watch is absorbed into the baseline and
	// never emitted, and the runtime silently runs a stale config.
	switch {
	case w.baselineHashSet:
		w.lastHash = w.baselineHash
	default:
		if h, err := fileHash(w.path); err == nil {
			w.lastHash = h
		}
	}

	switch w.mode {
	case ModePoll:
		w.startedOnce.Do(func() { close(w.started) })
		go w.pollLoop(ctx, ch)
	default:
		fsw, err := fsnotify.NewWatcher()
		if err != nil {
			w.running = false
			return nil, fmt.Errorf("file watcher: new fsnotify watcher: %w", err)
		}
		dir := filepath.Dir(w.path)
		if err := fsw.Add(dir); err != nil {
			_ = fsw.Close()
			w.running = false
			return nil, fmt.Errorf("file watcher: add %q: %w", dir, err)
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

// CoalescedReloads returns the number of parsed reloads that superseded a
// still-queued predecessor (I4). A non-zero value means the consumer is
// slower than the file changes; every reload is still eventually delivered as
// the latest config, none are silently dropped.
func (w *Watcher) CoalescedReloads() uint64 {
	return w.coalescedReloads.Load()
}

// notifyLoop uses fsnotify for file change detection with debouncing.
//
// Two hardening layers complement the raw fsnotify events:
//
//  1. Events are matched per-directory, not per-path. Kubernetes
//     ConfigMap updates atomically swap a "..data" symlink inside the
//     mount directory — the config file's own path never receives a
//     Write/Create/Rename, so an exact-path filter misses every update
//     forever. Any relevant event in the directory arms the debounce;
//     the content-hash gate in reloadIfChanged suppresses reloads when
//     unrelated files churned.
//  2. A slow resync ticker (resyncInterval) re-hashes the file
//     unconditionally, so an event fsnotify dropped (kernel queue
//     overflow) or never emitted is still applied within one interval.
func (w *Watcher) notifyLoop(ctx context.Context, fsw *fsnotify.Watcher, ch chan *ports.BridgeConfig) {
	w.runNotify(ctx, fsw.Events, fsw.Errors, fsw.Close, ch)
}

// runNotify is the notifyLoop body, split out so tests can inject
// event/error channels without racing a real fsnotify watcher's
// internal goroutines.
func (w *Watcher) runNotify(
	ctx context.Context,
	events <-chan fsnotify.Event,
	errs <-chan error,
	closeWatcher func() error,
	ch chan *ports.BridgeConfig,
) {
	var debounceTimer clock.Timer
	var debounceCh <-chan time.Time

	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		_ = closeWatcher()
		close(ch)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	resync := w.clk.NewTicker(w.resyncInterval)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return

		case event, ok := <-events:
			if !ok {
				return
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
			w.reloadIfChanged(ch)

		case <-resync.C():
			w.reloadIfChanged(ch)

		case err, ok := <-errs:
			if !ok {
				return
			}
			if w.logger != nil {
				w.logger.Warn("file config watcher: fsnotify error", "path", w.path, "error", err)
			}
			// Any watcher error — most importantly ErrEventOverflow,
			// which means the kernel dropped events — may have hidden a
			// config change. Force an immediate hash-check reload
			// instead of waiting for the next resync tick.
			w.reloadIfChanged(ch)
		}
	}
}

// pollLoop periodically reads the file and emits on content change.
// The content baseline (lastHash) is taken synchronously by Watch.
func (w *Watcher) pollLoop(ctx context.Context, ch chan *ports.BridgeConfig) {
	defer func() {
		close(ch)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	ticker := w.clk.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C():
			w.reloadIfChanged(ch)
		}
	}
}

// reloadIfChanged re-parses and delivers the config only when the file
// content actually differs from the last successfully delivered state.
// lastHash advances only after a successful parse and enqueue, so a
// transiently unreadable or syntactically broken file is retried on the
// next event/tick rather than being recorded as "seen".
func (w *Watcher) reloadIfChanged(ch chan *ports.BridgeConfig) {
	h, err := fileHash(w.path)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("file config watcher: read failed", "path", w.path, "error", err)
		}
		return
	}
	if h == w.lastHash {
		return
	}
	if w.emitParsed(ch) {
		w.lastHash = h
	}
}

// emitParsed parses the watched file and enqueues the result with
// latest-wins coalescing. It reports whether a config was delivered;
// parse failures are logged and return false.
func (w *Watcher) emitParsed(ch chan *ports.BridgeConfig) bool {
	cfg, err := parser.ParseFile(w.path, w.format, w.registry)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("file config watcher: parse failed", "path", w.path, "error", err)
		}
		return false
	}
	// Latest-wins coalescing (I4). The consumer channel is buffered to one.
	// Instead of silently discarding a valid reload when that slot is full —
	// which would leave the consumer stuck on a stale config — evict the
	// superseded pending config and enqueue the newest, so a slow consumer
	// always converges on the current file state. Only this single producer
	// goroutine touches the channel's send side, so the evict/enqueue pair
	// cannot race another writer and the loop terminates in at most a couple
	// of iterations.
	for {
		select {
		case ch <- cfg:
			now := w.clk.Now()
			w.lastApplied.Store(&now)
			return true
		default:
		}
		select {
		case <-ch:
			w.coalescedReloads.Add(1)
			if w.logger != nil {
				w.logger.Warn("file config watcher: superseded a queued reload before the consumer read it",
					"path", w.path, "coalesced_total", w.coalescedReloads.Load())
			}
		default:
			// Consumer drained the slot concurrently; retry the send.
		}
	}
}

func fileHash(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte

	f, err := os.Open(path)
	if err != nil {
		return sum, fmt.Errorf("file config watcher: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, fmt.Errorf("file config watcher: hash %q: %w", path, err)
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
