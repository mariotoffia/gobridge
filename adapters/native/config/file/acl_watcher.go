package file

import (
	"bytes"
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

	// readFile reads the watched file's bytes. It is a seam (default os.ReadFile)
	// so reloadIfChanged reads the file EXACTLY ONCE and derives both the change
	// hash and the parsed config from the SAME bytes — the read-once invariant a
	// test can pin down. Hashing one read and parsing another let a
	// truncated mid-write read be parsed and applied while lastHash recorded the
	// final content's hash, silently wedging a partial config.
	readFile func(string) ([]byte, error)

	mu      sync.Mutex
	running bool
	// stopping guards stopCh's close so two concurrent Stop calls cannot both
	// close it (a double close panics). running stays true until the loop's
	// deferred cleanup runs, so it cannot distinguish "already asked to stop"
	// from "still running"; stopping does. Reset in Watch on (re)start.
	stopping bool
	stopCh   chan struct{}
	// doneCh is closed by the watch loop when it fully exits. Stop waits on it
	// so a Stop-then-Watch cycle cannot leave the old loop alive alongside a new
	// one, both mutating lastHash — mirrors Manager.Stop.
	doneCh      chan struct{}
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

	// pendingHash/pendingSet hold a not-yet-confirmed content change for the
	// stability gate (torn in-place write). A change is applied only after the
	// SAME new bytes are read TWICE across the settle window (w.debounce): the
	// first read records the candidate here without emitting, and a confirming
	// read one window later commits it. A non-atomic in-place edit that briefly
	// leaves a truncated-but-parseable snapshot (valid YAML/JSON for only some
	// routes) therefore does not get applied — the confirming read sees the
	// content still changing (or settled to the full config) and the partial
	// state is never emitted. Operators who write atomically (rename) see only
	// the extra settle-window latency. Same single-goroutine confinement as
	// lastHash, so no lock is needed.
	pendingHash [sha256.Size]byte
	pendingSet  bool

	// readFailedLogged de-duplicates the read-failure WARN to once per
	// fail streak (reset by the next successful read). A start-empty
	// bridge legitimately watches a file that does not exist yet, and in
	// notify mode events are matched per-directory — every unrelated
	// change in a busy directory would otherwise re-log the same missing
	// file. Same single-goroutine confinement as lastHash.
	readFailedLogged bool

	// baselineHash, when baselineHashSet, seeds lastHash at Watch time instead
	// of hashing disk. Set via WithBaselineHash from the hash the caller
	// actually Loaded (Source.LoadHash), closing the Load↔Watch window where a
	// between-the-two edit would be absorbed into the baseline and never
	// emitted.
	baselineHash    [sha256.Size]byte
	baselineHashSet bool

	// coalescedReloads counts reloads whose predecessor was still queued and
	// had to be evicted so the newest config could be enqueued. It is a
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
	if w.readFile == nil {
		w.readFile = os.ReadFile
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
	w.doneCh = make(chan struct{})
	w.running = true
	w.stopping = false
	stopCh := w.stopCh
	doneCh := w.doneCh

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
	// New watch cycle, new fail streak: the first read failure after this
	// Watch must log even if a previous cycle ended mid-streak.
	w.readFailedLogged = false
	// A new watch cycle also starts the stability gate fresh: drop any
	// not-yet-confirmed candidate left by a previous cycle so a change is only
	// applied after being read twice WITHIN this cycle (relative to the baseline
	// just set above), never carried across a Stop/Watch restart.
	w.pendingSet = false

	switch w.mode {
	case ModePoll:
		w.startedOnce.Do(func() { close(w.started) })
		go w.pollLoop(ctx, ch, stopCh, doneCh)
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
		go w.notifyLoop(ctx, fsw, ch, stopCh, doneCh)
	}

	return ch, nil
}

// Stop stops the watcher and blocks until the watch loop has fully exited, so a
// Stop-then-Watch cycle never runs two loops that share lastHash.
func (w *Watcher) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	doneCh := w.doneCh
	if !w.stopping {
		w.stopping = true
		close(w.stopCh)
	}
	w.mu.Unlock()

	<-doneCh // both concurrent callers wait for the loop goroutine (and its deferred running=false) to finish
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
// still-queued predecessor. A non-zero value means the consumer is
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
func (w *Watcher) notifyLoop(ctx context.Context, fsw *fsnotify.Watcher, ch chan *ports.BridgeConfig, stopCh, doneCh chan struct{}) {
	w.runNotify(ctx, fsw.Events, fsw.Errors, fsw.Close, ch, stopCh, doneCh)
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
	stopCh, doneCh chan struct{},
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
		// Signal Stop LAST, after running=false, so a Stop that unblocks here
		// observes a fully torn-down watcher before it returns.
		close(doneCh)
	}()

	resync := w.clk.NewTicker(w.resyncInterval)
	defer resync.Stop()

	// armDebounce (re)starts the debounce timer. It doubles as the stability
	// settle window: when reloadIfChanged reports a not-yet-confirmed
	// change (pending), we re-arm here so the SAME bytes are re-read one window
	// later; only a change that reads identically twice is applied, holding a
	// torn mid-write snapshot back.
	armDebounce := func() {
		if debounceTimer == nil {
			debounceTimer = w.clk.NewTimer(w.debounce)
			debounceCh = debounceTimer.C()
		} else {
			debounceTimer.Reset(w.debounce)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return

		case event, ok := <-events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			armDebounce()

		case <-debounceCh:
			debounceTimer = nil
			debounceCh = nil
			// A pending (not-yet-stable) change re-arms the debounce so the
			// confirming re-read happens one settle window later.
			if w.reloadIfChanged(ch) {
				armDebounce()
			}

		case <-resync.C():
			if w.reloadIfChanged(ch) {
				armDebounce()
			}

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
			if w.reloadIfChanged(ch) {
				armDebounce()
			}
		}
	}
}

// pollLoop periodically reads the file and emits on content change.
// The content baseline (lastHash) is taken synchronously by Watch.
func (w *Watcher) pollLoop(ctx context.Context, ch chan *ports.BridgeConfig, stopCh, doneCh chan struct{}) {
	var confirmTimer clock.Timer
	var confirmCh <-chan time.Time

	defer func() {
		if confirmTimer != nil {
			confirmTimer.Stop()
		}
		close(ch)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(doneCh)
	}()

	ticker := w.clk.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// armConfirm schedules the stability re-read (the torn-write guard).
	// Poll mode has no debounce timer, so a dedicated one-shot timer (w.debounce)
	// provides the settle window across which a change must read identically
	// twice before it is applied.
	armConfirm := func() {
		if confirmTimer == nil {
			confirmTimer = w.clk.NewTimer(w.debounce)
			confirmCh = confirmTimer.C()
		} else {
			confirmTimer.Reset(w.debounce)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C():
			if w.reloadIfChanged(ch) {
				armConfirm()
			}
		case <-confirmCh:
			confirmTimer = nil
			confirmCh = nil
			if w.reloadIfChanged(ch) {
				armConfirm()
			}
		}
	}
}

// reloadIfChanged re-parses and delivers the config only when the file
// content actually differs from the last successfully delivered state AND has
// STABILIZED across the settle window. lastHash advances only after a
// successful parse and enqueue, so a transiently unreadable or syntactically
// broken file is retried on the next event/tick rather than being recorded as
// "seen".
//
// The file is read EXACTLY ONCE per attempt: the change hash and the parse both
// derive from the same byte slice. Hashing one read and parsing
// another let a truncating editor / slow copy be caught mid-write — parse
// succeeds on a truncated-but-valid prefix while lastHash records the FINAL
// content's hash, so the resync gate sees "no change" and the bridge silently
// runs a partial config. Mirrors Source.Load's read-once pattern.
//
// Stability gate (torn in-place write): a non-atomic write can briefly
// leave the file holding a truncated-but-parseable snapshot. A single read at
// that instant would parse and apply the partial config, dropping routes and
// triggering a real runtime swap. So a newly-changed content is NOT emitted on
// first sighting; it is recorded as a candidate and only applied once a second
// read across the settle window returns the SAME bytes. reloadIfChanged returns
// pending==true while a candidate awaits confirmation, so the watch loop
// re-reads one settle window later (the debounce timer in notify mode, a
// one-shot confirm timer in poll mode). A change that is still in flight keeps
// updating the candidate and is never applied until it settles. This does not
// substitute for atomic writes (a writer that pauses mid-write longer than the
// window can still settle on a partial file) — see the required atomic-rename
// discipline in docs/runbooks/external-config-atomic-writes.md — but it closes
// the common torn-read window.
func (w *Watcher) reloadIfChanged(ch chan *ports.BridgeConfig) (pending bool) {
	data, err := w.readFile(w.path)
	if err != nil {
		if w.logger != nil && !w.readFailedLogged {
			w.logger.Warn("file config watcher: read failed (logged once per fail streak)",
				"path", w.path, "error", err)
		}
		w.readFailedLogged = true
		// A read failure abandons any pending candidate: the next successful
		// read starts the stability gate over.
		w.pendingSet = false
		return false
	}
	w.readFailedLogged = false
	h := sha256.Sum256(data)
	if h == w.lastHash {
		// Content matches what is already applied (or the file settled back to
		// it mid-write); drop any half-written candidate we were tracking.
		w.pendingSet = false
		return false
	}
	if !w.pendingSet || h != w.pendingHash {
		// First sighting of this new content, or it changed again since the
		// previous read (writer still active). Hold it for stability
		// confirmation instead of applying a possibly torn snapshot.
		w.pendingHash = h
		w.pendingSet = true
		return true
	}
	// Same new content observed twice across the settle window → the write has
	// settled. Commit it: hash and parse derive from this single read.
	if w.emitParsed(data, ch) {
		w.lastHash = h
	}
	w.pendingSet = false
	return false
}

// emitParsed parses the supplied file bytes and enqueues the result with
// latest-wins coalescing. It reports whether a config was delivered;
// parse failures are logged and return false. It parses the SAME bytes
// reloadIfChanged hashed, never a fresh disk read.
func (w *Watcher) emitParsed(data []byte, ch chan *ports.BridgeConfig) bool {
	format := w.format
	if format == parser.FormatAuto || format == "" {
		format = detectSourceFormat(w.path)
	}
	cfg, err := parser.Parse(bytes.NewReader(data), format, w.registry)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("file config watcher: parse failed", "path", w.path, "error", err)
		}
		return false
	}
	// Latest-wins coalescing. The consumer channel is buffered to one.
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
