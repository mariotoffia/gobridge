package file

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// watcher handles file system watching for configuration changes.
type watcher struct {
	source     *Source
	fsWatcher  *fsnotify.Watcher
	changeCh   chan types.ConfigChange
	stopCh     chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	running    bool
	lastModMap map[string]time.Time // tracks last modification time per file
}

// newWatcher creates a new file watcher.
func newWatcher(source *Source) *watcher {
	return &watcher{
		source:     source,
		lastModMap: make(map[string]time.Time),
	}
}

// start begins watching for file changes.
func (w *watcher) start(ctx context.Context) (<-chan types.ConfigChange, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return w.changeCh, nil
	}

	w.changeCh = make(chan types.ConfigChange, 100)
	w.stopCh = make(chan struct{})
	w.running = true

	// Initialize last modification times
	w.initLastModTimes()

	if w.source.config.WatchInterval > 0 {
		// Use polling
		w.wg.Add(1)
		go w.pollLoop(ctx)
	} else {
		// Use fsnotify
		var err error
		w.fsWatcher, err = fsnotify.NewWatcher()
		if err != nil {
			w.running = false
			close(w.changeCh)
			return nil, err
		}

		// Add paths to watch
		if err := w.addWatchPaths(); err != nil {
			w.fsWatcher.Close()
			w.running = false
			close(w.changeCh)
			return nil, err
		}

		w.wg.Add(1)
		go w.fsnotifyLoop(ctx)
	}

	return w.changeCh, nil
}

// stop stops watching for file changes.
func (w *watcher) stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	close(w.stopCh)
	w.mu.Unlock()

	w.wg.Wait()

	if w.fsWatcher != nil {
		w.fsWatcher.Close()
		w.fsWatcher = nil
	}

	close(w.changeCh)
}

// initLastModTimes initializes the last modification time map.
func (w *watcher) initLastModTimes() {
	files, err := w.source.getConfigFiles()
	if err != nil {
		return
	}

	for _, f := range files {
		info, err := os.Stat(f)
		if err == nil {
			w.lastModMap[f] = info.ModTime()
		}
	}
}

// addWatchPaths adds paths to the fsnotify watcher.
func (w *watcher) addWatchPaths() error {
	path := w.source.config.Path

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		// Watch the directory
		if err := w.fsWatcher.Add(path); err != nil {
			return err
		}

		// If recursive, add subdirectories
		if w.source.config.Recursive {
			err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && p != path {
					return w.fsWatcher.Add(p)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	} else {
		// Watch the parent directory for the file
		if err := w.fsWatcher.Add(filepath.Dir(path)); err != nil {
			return err
		}
	}

	return nil
}

// fsnotifyLoop handles fsnotify events.
func (w *watcher) fsnotifyLoop(ctx context.Context) {
	defer w.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleFsEvent(event)
		case _, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			// Log error but continue watching
		}
	}
}

// handleFsEvent processes an fsnotify event.
func (w *watcher) handleFsEvent(event fsnotify.Event) {
	// Only care about JSON files
	if !isJSONFile(event.Name) {
		return
	}

	// Check if this file is within our watch scope
	if !w.isInScope(event.Name) {
		return
	}

	var changeType types.ConfigChangeType

	switch {
	case event.Op&fsnotify.Create == fsnotify.Create:
		changeType = types.ConfigChangeAdd
	case event.Op&fsnotify.Write == fsnotify.Write:
		changeType = types.ConfigChangeUpdate
	case event.Op&fsnotify.Remove == fsnotify.Remove:
		changeType = types.ConfigChangeDelete
	case event.Op&fsnotify.Rename == fsnotify.Rename:
		// Treat rename as delete (the new name will trigger a create)
		changeType = types.ConfigChangeDelete
	default:
		return
	}

	w.emitChange(event.Name, changeType)
}

// pollLoop handles polling-based watching.
func (w *watcher) pollLoop(ctx context.Context) {
	defer w.wg.Done()

	ticker := time.NewTicker(w.source.config.WatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkForChanges()
		}
	}
}

// checkForChanges checks for file changes during polling.
func (w *watcher) checkForChanges() {
	files, err := w.source.getConfigFiles()
	if err != nil {
		return
	}

	currentFiles := make(map[string]bool)

	for _, f := range files {
		currentFiles[f] = true

		info, err := os.Stat(f)
		if err != nil {
			continue
		}

		lastMod, exists := w.lastModMap[f]
		if !exists {
			// New file
			w.lastModMap[f] = info.ModTime()
			w.emitChange(f, types.ConfigChangeAdd)
		} else if info.ModTime().After(lastMod) {
			// Modified file
			w.lastModMap[f] = info.ModTime()
			w.emitChange(f, types.ConfigChangeUpdate)
		}
	}

	// Check for deleted files
	for f := range w.lastModMap {
		if !currentFiles[f] {
			delete(w.lastModMap, f)
			w.emitChange(f, types.ConfigChangeDelete)
		}
	}
}

// isInScope checks if a file is within the watch scope.
func (w *watcher) isInScope(path string) bool {
	configPath := w.source.config.Path

	info, err := os.Stat(configPath)
	if err != nil {
		return false
	}

	if !info.IsDir() {
		// Single file mode - check if it's the same file
		absPath, _ := filepath.Abs(path)
		absConfig, _ := filepath.Abs(configPath)
		return absPath == absConfig
	}

	// Directory mode - check if file is within directory
	absPath, _ := filepath.Abs(path)
	absConfig, _ := filepath.Abs(configPath)

	rel, err := filepath.Rel(absConfig, absPath)
	if err != nil {
		return false
	}

	// If recursive, any file under the directory is in scope
	// If not recursive, only direct children are in scope
	if !w.source.config.Recursive {
		return filepath.Dir(rel) == "."
	}

	return true
}

// emitChange sends a config change to the channel.
func (w *watcher) emitChange(path string, changeType types.ConfigChangeType) {
	var item types.ConfigItem

	if changeType != types.ConfigChangeDelete {
		parsed, err := parseFile(path)
		if err != nil {
			return
		}
		item = parsed
	} else {
		// For deletes, we need to create a minimal item from the cache
		w.source.mu.RLock()
		for _, cached := range w.source.items {
			if cached.FilePath == path {
				item = cached
				break
			}
		}
		w.source.mu.RUnlock()

		if item == nil {
			return
		}
	}

	change := types.ConfigChange{
		Type:      changeType,
		Item:      item,
		Timestamp: time.Now(),
	}

	select {
	case w.changeCh <- change:
	default:
		// Channel full, drop the change
	}
}
