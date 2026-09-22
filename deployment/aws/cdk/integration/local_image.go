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
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridgecdk"
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

// buildLocalRuntimeImage builds the runtime for the Docker daemon's platform,
// not for the platform the asset declares. The emulator runs every ECS task on
// the daemon's platform and ignores the task definition's RuntimePlatform, and
// since floci 2.1.0 it uses a local image only when the image was built for that
// platform; for any other image it pulls the tag instead, and a tag that exists
// only on this machine cannot be pulled. The task definition keeps the
// deployment's RuntimePlatform, X86_64 by default; only the image differs. On an
// arm64 host a run therefore starts an arm64 build of this checkout, and leaves
// unproven that the image for the declared platform starts.
func buildLocalRuntimeImage(t *testing.T, state *localBackend, asset localRuntimeAsset) string {
	t.Helper()
	platform := localRuntimePlatform(t, state)
	if override := strings.TrimSpace(os.Getenv(localImageEnv)); override != "" {
		verifyLocalRuntimeImage(t, override, platform, asset)
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
	t.Logf("runtime image %s: the deployment declares %s; building for %s, the Docker daemon's platform",
		image, asset.Platform, platform)
	cmd := exec.CommandContext(t.Context(), "docker", "build", "--platform", platform,
		"--build-arg", "INITIAL_CONFIG_FILE="+filepath.Base(file.Name()), "-t", image, root)
	cmd.Stdout, cmd.Stderr = testWriter{t}, testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("build local runtime with staged initial config: %v", err)
	}
	verifyLocalRuntimeImage(t, image, platform, asset)
	return image
}

// localRuntimePlatform returns the Docker daemon's platform, read once per run.
func localRuntimePlatform(t *testing.T, state *localBackend) string {
	t.Helper()
	if state.runtimePlatform == "" {
		out, err := dockerexec.Run(dockerexec.InspectTimeout, "info", "--format", "{{.OSType}}/{{.Architecture}}")
		if err != nil {
			t.Fatalf("read the Docker daemon platform: %v\n%s", err, out)
		}
		platform, err := daemonPlatform(string(out))
		if err != nil {
			t.Fatal(err)
		}
		state.runtimePlatform = platform
	}
	return state.runtimePlatform
}

// daemonPlatform normalizes `docker info`'s OSType/Architecture exactly as
// floci does before it compares a local image with it: lower case, aarch64 is
// arm64, x86_64 is amd64, and an unreported OS is linux. Only the two platforms
// a runtime image asset may declare are accepted.
func daemonPlatform(info string) (string, error) {
	reported := strings.TrimSpace(info)
	osType, arch, _ := strings.Cut(strings.ToLower(reported), "/")
	if osType == "" {
		osType = "linux"
	}
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	platform := osType + "/" + arch
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return "", fmt.Errorf("the Docker daemon reports the platform %q; the emulator runs ECS tasks only on "+
			"the daemon's platform, and the local runtime image is built only for linux/amd64 or linux/arm64",
			reported)
	}
	return platform, nil
}

func verifyLocalRuntimeImage(t *testing.T, image, platform string, asset localRuntimeAsset) {
	t.Helper()
	// No AWS calls or startup side effects: this command exits after hashing
	// its embedded bytes, before opening a config source or listening socket.
	// It runs on the platform the emulator will run the image on.
	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "--rm", "--network", "none",
		"--platform", platform, image, "-initial-config-digest")
	if err != nil {
		t.Fatalf("runtime image %q cannot verify its embedded initial config on %s: %v\n%s", image, platform, err, out)
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
