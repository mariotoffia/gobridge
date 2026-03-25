package dynamodb

import (
	"encoding/json"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Item implements types.ConfigItem for DynamoDB-backed configuration.
type Item struct {
	pk        string
	sk        string
	itemType  types.ConfigItemType
	version   int64
	data      any
	updatedAt time.Time
}

// GetPartitionKey returns the partition key for this item.
func (i *Item) GetPartitionKey() string {
	return i.pk
}

// GetSortKey returns the sort key for this item.
func (i *Item) GetSortKey() string {
	return i.sk
}

// GetType returns the type of this item.
func (i *Item) GetType() types.ConfigItemType {
	return i.itemType
}

// GetVersion returns the version of this item.
func (i *Item) GetVersion() int64 {
	return i.version
}

// GetData returns the data of this item.
func (i *Item) GetData() any {
	return i.data
}

// GetUpdatedAt returns when this item was last updated.
func (i *Item) GetUpdatedAt() time.Time {
	return i.updatedAt
}

// Ensure Item implements types.ConfigItem
var _ types.ConfigItem = (*Item)(nil)

// NewItem creates a new Item with the given values.
func NewItem(pk, sk string, itemType types.ConfigItemType, version int64, data any) *Item {
	return &Item{
		pk:        pk,
		sk:        sk,
		itemType:  itemType,
		version:   version,
		data:      data,
		updatedAt: time.Now(),
	}
}

// parseData parses a JSON string into a structured data type.
func parseData(dataStr string) any {
	if dataStr == "" {
		return nil
	}
	var data any
	if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
		return dataStr // Return as string if not valid JSON
	}
	return data
}
