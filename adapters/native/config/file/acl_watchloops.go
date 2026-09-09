package file

import (
	"context"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// notifyLoop watches directory events (including ConfigMap symlink swaps).
// Periodic reconciliation catches missed events and event-queue overflow.
func (w *Watcher) notifyLoop(ctx context.Context, fsw *fsnotify.Watcher, ch chan *ports.BridgeConfig, stopCh, doneCh chan struct{}) {
	w.runNotify(ctx, fsw.Events, fsw.Errors, fsw.Close, ch, stopCh, doneCh)
}

// runNotify accepts event channels so tests can order notification and read
// failures without racing the operating system's event delivery.
func (w *Watcher) runNotify(ctx context.Context, events <-chan fsnotify.Event, errs <-chan error,
	closeWatcher func() error, ch chan *ports.BridgeConfig, stopCh, doneCh chan struct{},
) {
	var debounceTimer clock.Timer
	var debounceCh <-chan time.Time
	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		if err := closeWatcher(); err != nil && w.logger != nil {
			w.logger.Warn("file config watcher: close failed", "error", err)
		}
		w.finishLoop(ch, doneCh)
	}()
	resync := w.clk.NewTicker(w.resyncInterval)
	defer resync.Stop()
	// The same timer debounces events and confirms that candidate bytes have
	// remained unchanged across a settle window.
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
				w.observeResult(nil, shared.ErrUnavailable.WithMessage("file watcher events closed"))
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				armDebounce()
			}
		case <-debounceCh:
			debounceTimer, debounceCh = nil, nil
			if w.reloadIfChanged(ch) {
				armDebounce()
			}
		case <-resync.C():
			if w.reloadIfChanged(ch) {
				armDebounce()
			}
		case err, ok := <-errs:
			if !ok {
				w.observeResult(nil, shared.ErrUnavailable.WithMessage("file watcher errors closed"))
				return
			}
			if err == nil {
				err = shared.ErrUnavailable.WithMessage("file watcher reported an unspecified error")
			}
			w.observeResult(nil, err)
			if w.logger != nil {
				w.logger.Warn("file config watcher: fsnotify error", "path", w.path, "error", err)
			}
			// Overflow may have hidden a config edit: reconcile immediately.
			if w.reloadIfChanged(ch) {
				armDebounce()
			}
		}
	}
}

// pollLoop uses the same stable-byte read and observation delivery as notify.
func (w *Watcher) pollLoop(ctx context.Context, ch chan *ports.BridgeConfig, stopCh, doneCh chan struct{}) {
	var confirmTimer clock.Timer
	var confirmCh <-chan time.Time
	defer func() {
		if confirmTimer != nil {
			confirmTimer.Stop()
		}
		w.finishLoop(ch, doneCh)
	}()
	ticker := w.clk.NewTicker(w.pollInterval)
	defer ticker.Stop()
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
			confirmTimer, confirmCh = nil, nil
			if w.reloadIfChanged(ch) {
				armConfirm()
			}
		}
	}
}

func (w *Watcher) finishLoop(ch chan *ports.BridgeConfig, doneCh chan struct{}) {
	close(ch)
	w.mu.Lock()
	w.closeObservation()
	w.running = false
	w.mu.Unlock()
	close(doneCh) // Stop observes complete teardown before another watch starts.
}
