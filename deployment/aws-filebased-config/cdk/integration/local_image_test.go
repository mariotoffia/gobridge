//go:build integration_local

package integration

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeImage_ReplacesOnlyBridgeImage(t *testing.T) {
	for _, image := range []string{"gobridge-filebased:config-one", "example.com/custom:local"} {
		t.Run(image, func(t *testing.T) {
			main := map[string]any{"Name": "gobridge", "Image": map[string]any{"Fn::Join": []any{}}}
			sidecar := map[string]any{"Name": "other", "Image": "other:tag"}
			props := map[string]any{"ContainerDefinitions": []any{main, sidecar}}
			require.NoError(t, useLocalRuntimeImage(props, image))
			require.Equal(t, image, main["Image"])
			require.Equal(t, "other:tag", sidecar["Image"])
		})
	}
	require.Error(t, useLocalRuntimeImage(map[string]any{}, "local:tag"))
}

func TestEmbeddedInitialConfig_ReadsCompleteGoenvPayload(t *testing.T) {
	payload := []byte("bridge:\n  id: local-bridge\n  description: " + strings.Repeat("x", 200*1024) + "\n")
	encoded := base64.StdEncoding.EncodeToString(payload)
	data := []byte("GOFLAGS=\"-ldflags=-s -w -X main.version=v0.0.0 -X main.gitSHA=module@v0.0.0 -X main.initialConfigBase64=" + encoded + "\"\n")
	got, err := embeddedInitialConfig(data)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestEmbeddedInitialConfig_RejectsMissingOrAmbiguousPayload(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"missing", `GOFLAGS="-ldflags=-s -w"`},
		{"empty", `GOFLAGS="-ldflags=-X main.initialConfigBase64="`},
		{"invalid", `GOFLAGS="-ldflags=-X main.initialConfigBase64=???"`},
		{"duplicate", `GOFLAGS="-ldflags=-X main.initialConfigBase64=eA== -X main.initialConfigBase64=eQ=="`},
		{"missing linker option", `GOFLAGS="-ldflags=main.initialConfigBase64=eA=="`},
		{"malformed quote", `GOFLAGS="-ldflags=-X main.initialConfigBase64=eA==`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := embeddedInitialConfig([]byte(tc.data))
			require.Error(t, err)
		})
	}
}

func TestLocalImageAssets_PreservesUnrelatedPublication(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"first", "second"} {
		asset := filepath.Join(dir, "asset."+id)
		require.NoError(t, os.Mkdir(asset, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(asset, "initial-config-"+initialConfigDigest([]byte(id))+".goenv"),
			[]byte(`GOFLAGS="-ldflags=-X main.initialConfigBase64=`+base64.StdEncoding.EncodeToString([]byte(id))+`"`+"\n"), 0o600))
	}
	manifest := map[string]any{
		"version": "48.0.0", "files": map[string]any{"lambda": map[string]any{"source": "unchanged"}},
		"dockerImages": map[string]any{
			"first":  imageManifestEntry("asset.first", "first"),
			"second": imageManifestEntry("asset.second", "second"),
			"other":  imageManifestEntry("asset.other", "other"),
		},
	}
	path := filepath.Join(dir, "Stack.assets.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	assets, err := readLocalImageAssets(dir, "Stack")
	require.NoError(t, err)
	for _, id := range []string{"first", "second"} {
		image := map[string]any{"Fn::Join": []any{"", []any{"account.dkr.ecr.", map[string]any{"Ref": "AWS::Region"}, ".amazonaws.com/repository:" + id}}}
		asset, err := assets.runtimeAsset(image)
		require.NoError(t, err)
		require.Equal(t, id, asset.ID)
		require.Equal(t, []byte(id), asset.Config)
		require.Equal(t, "linux/amd64", asset.Platform)
	}
	_, err = assets.runtimeAsset("other.registry/unknown:missing")
	require.Error(t, err)
	require.NoError(t, assets.save())
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	var after map[string]any
	require.NoError(t, json.Unmarshal(data, &after))
	require.Equal(t, manifest["files"], after["files"])
	require.Len(t, after["dockerImages"], 1, "only runtime assets replaced locally skip publication")
	require.Contains(t, after["dockerImages"], "other")
}

func TestLocalImageAssets_RejectsMismatchedConfigFilename(t *testing.T) {
	dir := t.TempDir()
	assetDir := filepath.Join(dir, "asset.runtime")
	require.NoError(t, os.Mkdir(assetDir, 0o700))
	flags := []byte(`GOFLAGS="-ldflags=-X main.initialConfigBase64=` + base64.StdEncoding.EncodeToString([]byte("changed config")) + `"` + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(assetDir, "initial-config-"+strings.Repeat("0", 64)+".goenv"), flags, 0o600))
	manifest := map[string]any{"dockerImages": map[string]any{
		"runtime": imageManifestEntry("asset.runtime", "runtime"),
	}}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Stack.assets.json"), data, 0o600))
	assets, err := readLocalImageAssets(dir, "Stack")
	require.NoError(t, err)
	_, err = assets.runtimeAsset("registry/repository:runtime")
	require.ErrorContains(t, err, "digest")
	require.Empty(t, assets.used, "an invalid asset must remain in the publication manifest")
}

func imageManifestEntry(directory, tag string) map[string]any {
	return map[string]any{
		"source":       map[string]any{"directory": directory, "platform": "linux/amd64"},
		"destinations": map[string]any{"local": map[string]any{"repositoryName": "repository", "imageTag": tag}},
	}
}

func TestLocalRuntimeImageOverride_RequiresMatchingEmbeddedDigest(t *testing.T) {
	payload := []byte("bridge:\n  id: local\n")
	digest := initialConfigDigest(payload)
	require.NoError(t, verifyInitialConfigDigest(payload, digest+"\n"))
	require.ErrorContains(t, verifyInitialConfigDigest(payload, strings.Repeat("0", 64)), "embedded initial config")
	require.Error(t, verifyInitialConfigDigest(payload, ""))
}

func TestIsRuntimeContainer_UsesDeclaredRuntimeName(t *testing.T) {
	require.True(t, isRuntimeContainer("gobridge"))
	require.False(t, isRuntimeContainer("metrics"))
}
