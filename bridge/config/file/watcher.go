package file

import (
	"context"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileWatcher watches a configuration file for changes.
// It uses fsnotify for file system notifications and debounces rapid changes.
type FileWatcher struct {
	path     string
	watcher  *fsnotify.Watcher
	debounce time.Duration

	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	eventsCh chan struct{}
}

// NewFileWatcher creates a new file watcher for the given path.
func NewFileWatcher(path string) (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &FileWatcher{
		path:     path,
		watcher:  watcher,
		debounce: 100 * time.Millisecond, // Debounce rapid changes
	}, nil
}

// Start begins watching the file for changes.
// Returns a channel that emits when the file changes.
func (w *FileWatcher) Start(ctx context.Context) (<-chan struct{}, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return w.eventsCh, nil
	}

	// Add the file to the watcher
	if err := w.watcher.Add(w.path); err != nil {
		return nil, err
	}

	w.stopCh = make(chan struct{})
	w.eventsCh = make(chan struct{}, 1)
	w.running = true

	go w.watchLoop(ctx)

	return w.eventsCh, nil
}

// watchLoop handles fsnotify events and debounces them.
func (w *FileWatcher) watchLoop(ctx context.Context) {
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		close(w.eventsCh)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Only care about write and create events
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			// Start or reset debounce timer
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(w.debounce)
				debounceCh = debounceTimer.C
			} else {
				debounceTimer.Reset(w.debounce)
			}

		case <-debounceCh:
			// Debounce period elapsed, emit change event
			select {
			case w.eventsCh <- struct{}{}:
			default:
				// Channel full, skip (previous event not yet processed)
			}
			debounceTimer = nil
			debounceCh = nil

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			// Log error but continue watching
			_ = err
		}
	}
}

// Stop stops watching the file.
func (w *FileWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}

	close(w.stopCh)
	w.watcher.Close()
	w.running = false
}

// SetDebounce sets the debounce duration for file changes.
func (w *FileWatcher) SetDebounce(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.debounce = d
}
