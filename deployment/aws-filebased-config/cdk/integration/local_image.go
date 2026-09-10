//go:build integration_local

package integration

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// Materialize the production config-bearing build asset, but never install its
// dummy module version. The local assembly rewrite builds this checkout with
// the staged payload and removes that asset from CDK's publication manifest.
//
//nolint:ireturn // BridgeImageSource is the sealed facade input.
func localRuntimeImageSource(_ awscdk.Stack) gobridgecdk.BridgeImageSource {
	return gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{Version: "v0.0.0"})
}

func runtimeContainer(properties map[string]any) (map[string]any, error) {
	containers, _ := properties["ContainerDefinitions"].([]any)
	var runtime map[string]any
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if ok && container["Name"] == "gobridge" {
			if runtime != nil {
				return nil, fmt.Errorf("task declares more than one gobridge container")
			}
			runtime = container
		}
	}
	if runtime == nil {
		return nil, fmt.Errorf("task declares no gobridge container")
	}
	return runtime, nil
}

func useLocalRuntimeImage(properties map[string]any, image string) error {
	container, err := runtimeContainer(properties)
	if err != nil {
		return err
	}
	container["Image"] = image
	return nil
}

func buildLocalRuntimeImage(t *testing.T, state *localBackend, asset localRuntimeAsset) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv(localImageEnv)); override != "" {
		verifyLocalRuntimeImage(t, override, asset)
		return override
	}
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatalf("resolve repository build context: %v", err)
	}
	file, err := os.CreateTemp(root, ".gobridge-initial-*.yaml")
	if err != nil {
		t.Fatalf("stage local embedded initial config: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	if _, err := file.Write(asset.Config); err != nil {
		_ = file.Close()
		t.Fatalf("write local embedded initial config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close local embedded initial config: %v", err)
	}
	image := "gobridge-local-runtime:" + strings.TrimPrefix(state.network, localRunPrefix) + "-" + asset.ID
	state.runtimeImages = append(state.runtimeImages, image)
	cmd := exec.CommandContext(t.Context(), "docker", "build", "--platform", asset.Platform,
		"--build-arg", "INITIAL_CONFIG_FILE="+filepath.Base(file.Name()), "-t", image, root)
	cmd.Stdout, cmd.Stderr = testWriter{t}, testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("build local runtime with staged initial config: %v", err)
	}
	verifyLocalRuntimeImage(t, image, asset)
	return image
}

func verifyLocalRuntimeImage(t *testing.T, image string, asset localRuntimeAsset) {
	t.Helper()
	// No AWS calls or startup side effects: this command exits after hashing
	// its embedded bytes, before opening a config source or listening socket.
	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "--rm", "--network", "none",
		"--platform", asset.Platform, image, "-initial-config-digest")
	if err != nil {
		t.Fatalf("runtime image %q cannot verify its embedded initial config: %v\n%s", image, err, out)
	}
	if err := verifyInitialConfigDigest(asset.Config, string(out)); err != nil {
		t.Fatalf("%s=%q: %v; unset %s to build this checkout with the fixture's config",
			localImageEnv, image, err, localImageEnv)
	}
}

func initialConfigDigest(config []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(config))
}

func verifyInitialConfigDigest(config []byte, output string) error {
	want := initialConfigDigest(config)
	if strings.TrimSpace(output) != want {
		return fmt.Errorf("runtime embedded initial config does not match the staged deployment (want digest %s)", want)
	}
	return nil
}
