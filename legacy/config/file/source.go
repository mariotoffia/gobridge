package file

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Source is a file-based implementation of types.ConfigSource.
// It reads configuration from JSON files on disk with optional watch support.
type Source struct {
	config  Config
	items   map[string]*ConfigItem // key: partitionKey:sortKey
	watcher *watcher
	mu      sync.RWMutex
}

// NewSource creates a new file-based ConfigSource.
func NewSource(path string, opts ...Option) (*Source, error) {
	s := &Source{
		config: Config{
			Path: path,
		},
		items: make(map[string]*ConfigItem),
	}

	for _, opt := range opts {
		opt(s)
	}

	// Validate path exists
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config path does not exist: %w", err)
	}

	s.watcher = newWatcher(s)

	return s, nil
}

// Discover performs initial configuration discovery.
// Called during bridge startup to load all relevant configuration.
func (s *Source) Discover(ctx context.Context) ([]types.ConfigItem, error) {
	files, err := s.getConfigFiles()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var items []types.ConfigItem

	for _, f := range files {
		item, err := parseFile(f)
		if err != nil {
			// Log error but continue with other files
			continue
		}

		key := s.itemKey(item.PartitionKey, item.SortKey)
		s.items[key] = item
		items = append(items, item)
	}

	return items, nil
}

// Watch returns a channel that receives configuration changes.
// The implementation uses fsnotify or polling based on configuration.
// The channel is closed when the context is cancelled.
func (s *Source) Watch(ctx context.Context) (<-chan types.ConfigChange, error) {
	return s.watcher.start(ctx)
}

// Get retrieves a specific configuration item.
func (s *Source) Get(ctx context.Context, partitionKey, sortKey string) (types.ConfigItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := s.itemKey(partitionKey, sortKey)
	item, ok := s.items[key]
	if !ok {
		return nil, types.ErrNotFound
	}

	return item, nil
}

// List retrieves all items matching the partition key.
func (s *Source) List(ctx context.Context, partitionKey string) ([]types.ConfigItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var items []types.ConfigItem

	for _, item := range s.items {
		if item.PartitionKey == partitionKey {
			items = append(items, item)
		}
	}

	return items, nil
}

// Write creates or updates a configuration item.
// For file-based storage, this writes to a file named after the partition and sort keys.
func (s *Source) Write(ctx context.Context, item types.ConfigItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Convert to our ConfigItem type
	configItem := &ConfigItem{
		PartitionKey: item.GetPartitionKey(),
		SortKey:      item.GetSortKey(),
		Type:         item.GetType(),
		Version:      item.GetVersion() + 1,
		Data:         item.GetData(),
		UpdatedAt:    time.Now(),
	}

	// Determine file path
	filePath := s.getItemFilePath(configItem)
	configItem.FilePath = filePath

	// Check version for optimistic locking
	key := s.itemKey(configItem.PartitionKey, configItem.SortKey)
	existing, exists := s.items[key]
	if exists {
		if item.GetVersion() > 0 && existing.Version != item.GetVersion() {
			return fmt.Errorf("version mismatch: expected %d, got %d", existing.Version, item.GetVersion())
		}
	} else if item.GetVersion() > 0 {
		return fmt.Errorf("item does not exist for update")
	}

	// Serialize and write
	data, err := serializeItem(configItem)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	s.items[key] = configItem
	return nil
}

// Delete removes a configuration item.
func (s *Source) Delete(ctx context.Context, partitionKey, sortKey string, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.itemKey(partitionKey, sortKey)
	existing, exists := s.items[key]
	if !exists {
		return types.ErrNotFound
	}

	// Check version for optimistic locking
	if version > 0 && existing.Version != version {
		return fmt.Errorf("version mismatch: expected %d, got %d", existing.Version, version)
	}

	// Delete the file
	if err := os.Remove(existing.FilePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete config file: %w", err)
	}

	delete(s.items, key)
	return nil
}

// Close stops watching and releases resources.
func (s *Source) Close() error {
	s.watcher.stop()
	return nil
}

// getConfigFiles returns all JSON config files from the configured path.
func (s *Source) getConfigFiles() ([]string, error) {
	info, err := os.Stat(s.config.Path)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return scanDirectory(s.config.Path, s.config.Recursive)
	}

	// Single file
	if !isJSONFile(s.config.Path) {
		return nil, fmt.Errorf("config file must be a JSON file: %s", s.config.Path)
	}

	return []string{s.config.Path}, nil
}

// itemKey creates a unique key for a config item.
func (s *Source) itemKey(partitionKey, sortKey string) string {
	return partitionKey + ":" + sortKey
}

// getItemFilePath determines the file path for a config item.
func (s *Source) getItemFilePath(item *ConfigItem) string {
	if item.FilePath != "" {
		return item.FilePath
	}

	info, _ := os.Stat(s.config.Path)
	if info != nil && !info.IsDir() {
		// Single file mode - use the configured path
		return s.config.Path
	}

	// Directory mode - create a file name from partition and sort keys
	fileName := fmt.Sprintf("%s_%s.json", item.PartitionKey, item.SortKey)
	return s.config.Path + "/" + fileName
}

// Ensure Source implements the required interfaces
var _ types.ConfigSource = (*Source)(nil)
var _ types.ConfigWriter = (*Source)(nil)
