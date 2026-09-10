//go:build integration_local

package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type localRuntimeAsset struct {
	ID, Platform string
	Config       []byte
}

type localImageAssets struct {
	path     string
	manifest map[string]any
	used     map[string]bool
}

func readLocalImageAssets(directory, stack string) (*localImageAssets, error) {
	path := filepath.Join(directory, stack+".assets.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image asset manifest: %w", err)
	}
	assets := &localImageAssets{path: path, used: map[string]bool{}}
	if err := json.Unmarshal(data, &assets.manifest); err != nil {
		return nil, fmt.Errorf("parse image asset manifest: %w", err)
	}
	return assets, nil
}

func (a *localImageAssets) runtimeAsset(image any) (localRuntimeAsset, error) {
	suffix, _ := image.(string)
	if intrinsic, ok := image.(map[string]any); ok {
		suffix, _ = intrinsic["Fn::Sub"].(string)
		parts, _ := intrinsic["Fn::Join"].([]any)
		if len(parts) == 2 && parts[0] == "" {
			values, _ := parts[1].([]any)
			if len(values) > 0 {
				suffix, _ = values[len(values)-1].(string)
			}
		}
	}
	var result localRuntimeAsset
	var source map[string]any
	images, _ := a.manifest["dockerImages"].(map[string]any)
	for id, raw := range images {
		entry, _ := raw.(map[string]any)
		destinations, _ := entry["destinations"].(map[string]any)
		for _, rawDestination := range destinations {
			destination, _ := rawDestination.(map[string]any)
			tag, _ := destination["imageTag"].(string)
			if tag == "" || !strings.HasSuffix(suffix, ":"+tag) {
				continue
			}
			if result.ID != "" && result.ID != id {
				return result, fmt.Errorf("runtime image matches multiple staged assets")
			}
			result.ID = id
			source, _ = entry["source"].(map[string]any)
		}
	}
	if result.ID == "" {
		return result, fmt.Errorf("runtime image has no matching staged Go build asset")
	}
	directory, _ := source["directory"].(string)
	if directory == "" || filepath.IsAbs(directory) || !filepath.IsLocal(directory) {
		return result, fmt.Errorf("runtime image asset has no assembly-local build directory")
	}
	result.Platform, _ = source["platform"].(string)
	if result.Platform != "linux/amd64" && result.Platform != "linux/arm64" {
		return result, fmt.Errorf("runtime image asset has unsupported platform %q", result.Platform)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(a.path), directory, "initial-config-*.base64"))
	if err != nil || len(files) != 1 {
		return result, fmt.Errorf("runtime image asset must stage exactly one initial config, found %d: %v", len(files), err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		return result, fmt.Errorf("read staged initial config: %w", err)
	}
	result.Config, err = embeddedInitialConfig(data)
	if err != nil {
		return result, err
	}
	if filepath.Base(files[0]) != "initial-config-"+initialConfigDigest(result.Config)+".base64" {
		return result, fmt.Errorf("staged initial config filename must contain the raw payload's SHA-256 digest")
	}
	a.used[result.ID] = true
	return result, nil
}

func (a *localImageAssets) save() error {
	images, _ := a.manifest["dockerImages"].(map[string]any)
	for id := range a.used {
		delete(images, id)
	}
	data, err := json.MarshalIndent(a.manifest, "", " ")
	if err != nil {
		return fmt.Errorf("encode local image manifest: %w", err)
	}
	return os.WriteFile(a.path, data, 0o600)
}

func embeddedInitialConfig(data []byte) ([]byte, error) {
	payload, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode staged embedded initial config: %w", err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("staged build must contain a nonempty embedded initial config")
	}
	return payload, nil
}
