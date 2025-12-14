package file

import (
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ConfigItem is a file-based implementation of types.ConfigItem.
type ConfigItem struct {
	// PartitionKey is the partition key for this item.
	// Example: "cluster:default", "bridge:bridge-1", "pipeline:mqtt-to-sqs"
	PartitionKey string `json:"partitionKey"`
	// SortKey is the sort key for this item.
	// Example: "topic:sensors/temperature", "subscription:queue-1"
	SortKey string `json:"sortKey"`
	// Type is the type of configuration item.
	Type types.ConfigItemType `json:"type"`
	// Version is the version of this item (for optimistic locking).
	Version int64 `json:"version"`
	// Data is the actual configuration data.
	Data any `json:"data"`
	// UpdatedAt is when this item was last updated.
	UpdatedAt time.Time `json:"updatedAt"`
	// FilePath is the path to the file this item was loaded from.
	// This is not serialized to JSON.
	FilePath string `json:"-"`
}

// GetPartitionKey returns the partition key for this item.
func (c *ConfigItem) GetPartitionKey() string {
	return c.PartitionKey
}

// GetSortKey returns the sort key for this item.
func (c *ConfigItem) GetSortKey() string {
	return c.SortKey
}

// GetType returns the type of configuration item.
func (c *ConfigItem) GetType() types.ConfigItemType {
	return c.Type
}

// GetVersion returns the version of this item.
func (c *ConfigItem) GetVersion() int64 {
	return c.Version
}

// GetData returns the actual configuration data.
func (c *ConfigItem) GetData() any {
	return c.Data
}

// GetUpdatedAt returns when this item was last updated.
func (c *ConfigItem) GetUpdatedAt() time.Time {
	return c.UpdatedAt
}

// Ensure ConfigItem implements types.ConfigItem
var _ types.ConfigItem = (*ConfigItem)(nil)
