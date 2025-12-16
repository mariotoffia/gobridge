package file

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"gopkg.in/yaml.v3"
)

// Parser handles parsing of configuration files.
type Parser struct {
	format Format
}

// NewParser creates a new configuration parser.
func NewParser(format Format) *Parser {
	return &Parser{format: format}
}

// ParseFile reads and parses a configuration file.
func (p *Parser) ParseFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	format := p.format
	if format == FormatAuto {
		format = detectFormat(path)
	}

	return p.Parse(data, format)
}

// Parse parses configuration data in the specified format.
func (p *Parser) Parse(data []byte, format Format) (*FileConfig, error) {
	var config FileConfig

	switch format {
	case FormatYAML:
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}
	case FormatJSON:
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse JSON: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}

	return &config, nil
}

// detectFormat detects the configuration format from the file extension.
func detectFormat(path string) Format {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		return FormatYAML
	case ".json":
		return FormatJSON
	default:
		// Default to YAML
		return FormatYAML
	}
}

// ToConfigItems converts a FileConfig to a slice of ConfigItem.
func ToConfigItems(config *FileConfig, fileModTime time.Time) []types.ConfigItem {
	var items []types.ConfigItem
	version := fileModTime.UnixNano()

	// Add bridge config item
	if config.Bridge.ID != "" {
		items = append(items, &fileConfigItem{
			partitionKey: "bridge:" + config.Bridge.ID,
			sortKey:      "settings",
			itemType:     types.ConfigItemType("bridge"),
			version:      version,
			data:         config.Bridge,
			updatedAt:    fileModTime,
		})
	}

	// Add connection config items
	for _, conn := range config.Connections {
		items = append(items, &fileConfigItem{
			partitionKey: "connection:" + conn.ID,
			sortKey:      "settings",
			itemType:     types.ConfigItemTypeConnection,
			version:      version,
			data:         conn,
			updatedAt:    fileModTime,
		})
	}

	// Add pipeline config items
	for _, pipeline := range config.Pipelines {
		items = append(items, &fileConfigItem{
			partitionKey: "pipeline:" + pipeline.ID,
			sortKey:      "settings",
			itemType:     types.ConfigItemTypePipeline,
			version:      version,
			data:         pipeline,
			updatedAt:    fileModTime,
		})
	}

	// Add route config items
	for _, route := range config.Routes {
		items = append(items, &fileConfigItem{
			partitionKey: "route:" + route.ID,
			sortKey:      "settings",
			itemType:     types.ConfigItemTypeRoute,
			version:      version,
			data:         route,
			updatedAt:    fileModTime,
		})
	}

	return items
}

// itemKey generates a unique key for a config item.
func itemKey(item types.ConfigItem) string {
	return item.GetPartitionKey() + ":" + item.GetSortKey()
}

// ComputeChanges computes the changes between old and new config items.
func ComputeChanges(oldItems, newItems []types.ConfigItem) []types.ConfigChange {
	var changes []types.ConfigChange
	now := time.Now()

	// Build maps for comparison
	oldMap := make(map[string]types.ConfigItem)
	for _, item := range oldItems {
		oldMap[itemKey(item)] = item
	}

	newMap := make(map[string]types.ConfigItem)
	for _, item := range newItems {
		newMap[itemKey(item)] = item
	}

	// Find added and updated items
	for key, newItem := range newMap {
		oldItem, exists := oldMap[key]
		if !exists {
			// Added
			changes = append(changes, types.ConfigChange{
				Type:      types.ConfigChangeAdd,
				Item:      newItem,
				Timestamp: now,
			})
		} else if oldItem.GetVersion() != newItem.GetVersion() {
			// Updated (version changed = file was modified)
			changes = append(changes, types.ConfigChange{
				Type:      types.ConfigChangeUpdate,
				Item:      newItem,
				Timestamp: now,
			})
		}
	}

	// Find deleted items
	for key, oldItem := range oldMap {
		if _, exists := newMap[key]; !exists {
			changes = append(changes, types.ConfigChange{
				Type:      types.ConfigChangeDelete,
				Item:      oldItem,
				Timestamp: now,
			})
		}
	}

	return changes
}
