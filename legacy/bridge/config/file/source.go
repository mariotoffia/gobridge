package file

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// FileConfigSource implements types.ConfigSource for file-based configuration.
// It loads configuration from YAML or JSON files and optionally watches for changes.
type FileConfigSource struct {
	path         string
	format       Format
	watchEnabled bool

	// parser handles file parsing
	parser *Parser

	// watcher handles file change detection
	watcher *FileWatcher

	// items stores the current configuration items
	items []types.ConfigItem

	// itemMap provides fast lookup by key
	itemMap map[string]types.ConfigItem

	mu sync.RWMutex
}

// Option configures a FileConfigSource.
type Option func(*FileConfigSource)

// WithFormat sets the configuration file format.
// If not set, the format is auto-detected from the file extension.
func WithFormat(format Format) Option {
	return func(s *FileConfigSource) {
		s.format = format
	}
}

// WithWatch enables file watching for hot-reload.
func WithWatch(enabled bool) Option {
	return func(s *FileConfigSource) {
		s.watchEnabled = enabled
	}
}

// NewConfigSource creates a new file-based configuration source.
func NewConfigSource(path string, opts ...Option) (*FileConfigSource, error) {
	s := &FileConfigSource{
		path:    path,
		format:  FormatAuto,
		itemMap: make(map[string]types.ConfigItem),
	}

	for _, opt := range opts {
		opt(s)
	}

	s.parser = NewParser(s.format)

	// Verify the file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s", path)
	}

	return s, nil
}

// Discover performs initial configuration discovery.
// This loads and parses the configuration file.
func (s *FileConfigSource) Discover(ctx context.Context) ([]types.ConfigItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, err := s.loadConfig()
	if err != nil {
		return nil, err
	}

	s.items = items
	s.itemMap = make(map[string]types.ConfigItem, len(items))
	for _, item := range items {
		s.itemMap[itemKey(item)] = item
	}

	return items, nil
}

// Watch returns a channel that receives configuration changes.
// If watching is not enabled, this returns an error.
func (s *FileConfigSource) Watch(ctx context.Context) (<-chan types.ConfigChange, error) {
	if !s.watchEnabled {
		return nil, fmt.Errorf("file watching is not enabled")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Create watcher if not exists
	if s.watcher == nil {
		watcher, err := NewFileWatcher(s.path)
		if err != nil {
			return nil, fmt.Errorf("failed to create file watcher: %w", err)
		}
		s.watcher = watcher
	}

	// Create change channel
	changeCh := make(chan types.ConfigChange, 100)

	// Capture watcher reference to avoid race with Close()
	watcher := s.watcher

	// Start watching in background
	go s.watchLoop(ctx, changeCh, watcher)

	return changeCh, nil
}

// watchLoop monitors file changes and emits ConfigChange events.
func (s *FileConfigSource) watchLoop(ctx context.Context, changeCh chan<- types.ConfigChange, watcher *FileWatcher) {
	defer close(changeCh)

	if watcher == nil {
		return
	}

	events, err := watcher.Start(ctx)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			watcher.Stop()
			return
		case _, ok := <-events:
			if !ok {
				return
			}

			// Reload configuration
			newItems, err := s.loadConfig()
			if err != nil {
				// Log error but continue watching
				continue
			}

			// Compute changes
			s.mu.RLock()
			oldItems := s.items
			s.mu.RUnlock()

			changes := ComputeChanges(oldItems, newItems)

			// Update stored items
			s.mu.Lock()
			s.items = newItems
			s.itemMap = make(map[string]types.ConfigItem, len(newItems))
			for _, item := range newItems {
				s.itemMap[itemKey(item)] = item
			}
			s.mu.Unlock()

			// Emit changes
			for _, change := range changes {
				select {
				case changeCh <- change:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// Get retrieves a specific configuration item.
func (s *FileConfigSource) Get(ctx context.Context, partitionKey, sortKey string) (types.ConfigItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := partitionKey + ":" + sortKey
	item, ok := s.itemMap[key]
	if !ok {
		return nil, types.ErrNotFound
	}

	return item, nil
}

// List retrieves all items matching the partition key.
func (s *FileConfigSource) List(ctx context.Context, partitionKey string) ([]types.ConfigItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []types.ConfigItem
	for key, item := range s.itemMap {
		// Check if key starts with partition key
		if len(key) >= len(partitionKey) && key[:len(partitionKey)] == partitionKey {
			result = append(result, item)
		}
	}

	return result, nil
}

// loadConfig loads and parses the configuration file.
func (s *FileConfigSource) loadConfig() ([]types.ConfigItem, error) {
	// Get file modification time
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat config file: %w", err)
	}

	// Parse the file
	config, err := s.parser.ParseFile(s.path)
	if err != nil {
		return nil, err
	}

	// Convert to config items
	return ToConfigItems(config, info.ModTime()), nil
}

// Path returns the configuration file path.
func (s *FileConfigSource) Path() string {
	return s.path
}

// Reload forces a reload of the configuration file.
// This is useful for manual reload triggers.
func (s *FileConfigSource) Reload(ctx context.Context) ([]types.ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldItems := s.items

	newItems, err := s.loadConfig()
	if err != nil {
		return nil, err
	}

	changes := ComputeChanges(oldItems, newItems)

	s.items = newItems
	s.itemMap = make(map[string]types.ConfigItem, len(newItems))
	for _, item := range newItems {
		s.itemMap[itemKey(item)] = item
	}

	return changes, nil
}

// Close stops watching and releases resources.
func (s *FileConfigSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.watcher != nil {
		s.watcher.Stop()
		s.watcher = nil
	}

	return nil
}

// Ensure FileConfigSource implements types.ConfigSource
var _ types.ConfigSource = (*FileConfigSource)(nil)
