package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ConfigReloader handles runtime configuration reloading from config sources.
type ConfigReloader struct {
	mu sync.RWMutex

	// sources are the config sources to reload from
	sources []types.ConfigSource

	// handler processes configuration changes
	handler types.ConfigChangeHandler

	// bridge is the bridge to update
	bridge *Bridge

	// lastConfig stores the last known configuration for diffing
	lastConfig map[string]types.ConfigItem

	// watching indicates if we're actively watching for changes
	watching bool

	// stopCh signals the watcher to stop
	stopCh chan struct{}

	// pollInterval for polling-based sources
	pollInterval time.Duration

	// Log is the LogCreator for logging (optional)
	Log types.LogCreator

	// onReload is called after a successful reload
	onReload func(result *ReloadResult)
}

// ReloadResult contains the result of a configuration reload.
type ReloadResult struct {
	// Timestamp of the reload
	Timestamp time.Time `json:"timestamp"`
	// Source that triggered the reload (empty if manual)
	Source string `json:"source,omitempty"`
	// ChangesApplied is the number of changes applied
	ChangesApplied int `json:"changesApplied"`
	// Added items
	Added []string `json:"added,omitempty"`
	// Updated items
	Updated []string `json:"updated,omitempty"`
	// Deleted items
	Deleted []string `json:"deleted,omitempty"`
	// Errors encountered
	Errors []string `json:"errors,omitempty"`
	// Duration of the reload operation
	Duration time.Duration `json:"duration"`
}

// ConfigReloaderConfig configures the config reloader.
type ConfigReloaderConfig struct {
	// PollInterval for polling-based sources
	PollInterval time.Duration `json:"pollInterval,omitempty"`
	// OnReload callback after successful reload
	OnReload func(result *ReloadResult) `json:"-"`
}

// ConfigReloaderOption configures a ConfigReloader.
type ConfigReloaderOption func(*ConfigReloader)

// WithPollInterval sets the poll interval.
func ReloaderWithPollInterval(interval time.Duration) ConfigReloaderOption {
	return func(r *ConfigReloader) {
		r.pollInterval = interval
	}
}

// ReloaderWithLoggerFactory sets the logger factory.
// The reloader will create its own LogCreator using factory("config-reloader").
func ReloaderWithLoggerFactory(factory types.LoggerFactory) ConfigReloaderOption {
	return func(r *ConfigReloader) {
		if factory != nil {
			r.Log = factory("config-reloader")
		}
	}
}

// WithOnReload sets the reload callback.
func ReloaderWithOnReload(fn func(result *ReloadResult)) ConfigReloaderOption {
	return func(r *ConfigReloader) {
		r.onReload = fn
	}
}

// NewConfigReloader creates a new config reloader.
func NewConfigReloader(
	bridge *Bridge,
	handler types.ConfigChangeHandler,
	sources []types.ConfigSource,
	opts ...ConfigReloaderOption,
) *ConfigReloader {
	r := &ConfigReloader{
		bridge:       bridge,
		handler:      handler,
		sources:      sources,
		lastConfig:   make(map[string]types.ConfigItem),
		pollInterval: 30 * time.Second,
		stopCh:       make(chan struct{}),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Reload triggers an immediate configuration reload.
func (r *ConfigReloader) Reload(ctx context.Context) (*ReloadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	start := time.Now()
	result := &ReloadResult{
		Timestamp: start,
	}

	// Load current configuration from all sources
	currentConfig := make(map[string]types.ConfigItem)
	for _, source := range r.sources {
		// Use Discover for full configuration load
		items, err := source.Discover(ctx)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("source %T: %v", source, err))
			continue
		}
		for _, item := range items {
			key := configItemKey(item)
			currentConfig[key] = item
		}
	}

	// Compute diff
	changes := r.computeChanges(currentConfig)

	// Apply changes
	for _, change := range changes {
		if err := r.applyChange(ctx, change); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s %s: %v",
				change.Type, configItemKey(change.Item), err))
		} else {
			key := configItemKey(change.Item)
			switch change.Type {
			case types.ConfigChangeAdd:
				result.Added = append(result.Added, key)
			case types.ConfigChangeUpdate:
				result.Updated = append(result.Updated, key)
			case types.ConfigChangeDelete:
				result.Deleted = append(result.Deleted, key)
			}
			result.ChangesApplied++
		}
	}

	// Update last config
	r.lastConfig = currentConfig

	result.Duration = time.Since(start)

	if r.onReload != nil {
		r.onReload(result)
	}

	return result, nil
}

// computeChanges computes the diff between last and current config.
func (r *ConfigReloader) computeChanges(current map[string]types.ConfigItem) []types.ConfigChange {
	var changes []types.ConfigChange

	// Find added and updated items
	for key, item := range current {
		oldItem, exists := r.lastConfig[key]
		if !exists {
			changes = append(changes, types.ConfigChange{
				Type: types.ConfigChangeAdd,
				Item: item,
			})
		} else if !configItemEqual(oldItem, item) {
			changes = append(changes, types.ConfigChange{
				Type: types.ConfigChangeUpdate,
				Item: item,
			})
		}
	}

	// Find deleted items
	for key, item := range r.lastConfig {
		if _, exists := current[key]; !exists {
			changes = append(changes, types.ConfigChange{
				Type: types.ConfigChangeDelete,
				Item: item,
			})
		}
	}

	return changes
}

// applyChange applies a single configuration change.
func (r *ConfigReloader) applyChange(ctx context.Context, change types.ConfigChange) error {
	if r.handler == nil {
		return fmt.Errorf("no config change handler configured")
	}

	switch change.Item.GetType() {
	case types.ConfigItemTypeConnection:
		return r.handler.HandleConnectionChange(ctx, change)
	case types.ConfigItemTypeSource:
		return r.handler.HandleSourceChange(ctx, change)
	case types.ConfigItemTypeTarget:
		return r.handler.HandleTargetChange(ctx, change)
	case types.ConfigItemTypePipeline:
		return r.handlePipelineChange(ctx, change)
	default:
		return fmt.Errorf("unknown config item type: %s", change.Item.GetType())
	}
}

// handlePipelineChange handles pipeline add/update/delete.
func (r *ConfigReloader) handlePipelineChange(ctx context.Context, change types.ConfigChange) error {
	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.PipelineConfig)
		if !ok {
			return fmt.Errorf("invalid pipeline config data")
		}
		pipeline, err := r.bridge.CreatePipelineFromConfig(ctx, config)
		if err != nil {
			return err
		}
		return r.bridge.AddPipelineRunning(ctx, pipeline)

	case types.ConfigChangeDelete:
		pipelineID := extractPipelineID(change.Item.GetPartitionKey())
		return r.bridge.RemovePipelineRunning(ctx, pipelineID)

	case types.ConfigChangeUpdate:
		// Update = remove + add
		config, ok := change.Item.GetData().(types.PipelineConfig)
		if !ok {
			return fmt.Errorf("invalid pipeline config data")
		}
		pipelineID := extractPipelineID(change.Item.GetPartitionKey())
		if err := r.bridge.RemovePipelineRunning(ctx, pipelineID); err != nil {
			// Ignore not found errors for updates
			if err != types.ErrNotFound {
				return err
			}
		}
		pipeline, err := r.bridge.CreatePipelineFromConfig(ctx, config)
		if err != nil {
			return err
		}
		return r.bridge.AddPipelineRunning(ctx, pipeline)

	default:
		return fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// StartWatching starts watching config sources for changes.
func (r *ConfigReloader) StartWatching(ctx context.Context) error {
	r.mu.Lock()
	if r.watching {
		r.mu.Unlock()
		return fmt.Errorf("already watching")
	}
	r.watching = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	// Start watchers for each source that supports it
	for _, source := range r.sources {
		if watcher, ok := source.(types.ConfigWatcher); ok {
			go r.watchSource(ctx, watcher)
		}
	}

	// Start polling for sources that don't support watching
	go r.pollLoop(ctx)

	return nil
}

// StopWatching stops watching for configuration changes.
func (r *ConfigReloader) StopWatching() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.watching {
		return
	}

	close(r.stopCh)
	r.watching = false
}

// watchSource watches a single config source for changes.
func (r *ConfigReloader) watchSource(ctx context.Context, watcher types.ConfigWatcher) {
	changes, err := watcher.Watch(ctx)
	if err != nil {
		if r.Log != nil {
			r.Log(ctx, types.LogLevelError).Err(err).Msg("failed to start watcher")
		}
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case change, ok := <-changes:
			if !ok {
				return
			}
			r.mu.Lock()
			if err := r.applyChange(ctx, change); err != nil {
				if r.Log != nil {
					r.Log(ctx, types.LogLevelError).Err(err).Msg("failed to apply config change")
				}
			} else {
				// Update last config
				key := configItemKey(change.Item)
				if change.Type == types.ConfigChangeDelete {
					delete(r.lastConfig, key)
				} else {
					r.lastConfig[key] = change.Item
				}
			}
			r.mu.Unlock()
		}
	}
}

// pollLoop periodically reloads configuration.
func (r *ConfigReloader) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			if _, err := r.Reload(ctx); err != nil {
				if r.Log != nil {
					r.Log(ctx, types.LogLevelError).Err(err).Msg("config reload failed")
				}
			}
		}
	}
}

// configItemKey generates a unique key for a config item.
func configItemKey(item types.ConfigItem) string {
	return fmt.Sprintf("%s:%s:%s", item.GetType(), item.GetPartitionKey(), item.GetSortKey())
}

// extractPipelineID extracts pipeline ID from partition key.
// Expected format: "pipeline:{id}"
func extractPipelineID(partitionKey string) string {
	const prefix = "pipeline:"
	if len(partitionKey) > len(prefix) && partitionKey[:len(prefix)] == prefix {
		return partitionKey[len(prefix):]
	}
	return partitionKey
}

// configItemEqual compares two config items for equality.
func configItemEqual(a, b types.ConfigItem) bool {
	if a.GetPartitionKey() != b.GetPartitionKey() {
		return false
	}
	if a.GetSortKey() != b.GetSortKey() {
		return false
	}
	if a.GetVersion() != b.GetVersion() {
		return false
	}
	// Data comparison would need deep equality
	return true
}

// ConfigWatcher extends ConfigSource to support watching for changes.
// This interface should be in types but defined here for convenience.
type ConfigWatcher interface {
	types.ConfigSource
	Watch(ctx context.Context) (<-chan types.ConfigChange, error)
}
