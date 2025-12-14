package file

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseFile reads and parses a JSON configuration file.
func parseFile(path string) (*ConfigItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	return parseJSON(data, path)
}

// parseJSON parses JSON data into a ConfigItem.
func parseJSON(data []byte, filePath string) (*ConfigItem, error) {
	var item ConfigItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("failed to parse JSON from %s: %w", filePath, err)
	}

	item.FilePath = filePath
	return &item, nil
}

// isJSONFile checks if a file has a JSON extension.
func isJSONFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".json"
}

// scanDirectory scans a directory for JSON files.
func scanDirectory(dir string, recursive bool) ([]string, error) {
	var files []string

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip directories unless we're at the root
		if d.IsDir() {
			if path != dir && !recursive {
				return filepath.SkipDir
			}
			return nil
		}

		// Only include JSON files
		if isJSONFile(path) {
			files = append(files, path)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to scan directory %s: %w", dir, err)
	}

	return files, nil
}

// serializeItem serializes a ConfigItem to JSON.
func serializeItem(item *ConfigItem) ([]byte, error) {
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to serialize config item: %w", err)
	}
	return data, nil
}
