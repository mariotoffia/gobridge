package imgsource_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func stagedInitialConfig(t *testing.T, cfg *ports.BridgeConfig) (string, string) {
	t.Helper()
	out := t.TempDir()
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(out)})
	stack := awscdk.NewStack(app, jsii.String("Embedded"), nil)
	image := imgsource.NewGoBuild(imgsource.GoBuildProps{Version: "v0.4.0"}).
		Materialize(stack, jsii.String("Image"), cfg)
	task := awsecs.NewFargateTaskDefinition(stack, jsii.String("Task"), nil)
	task.AddContainer(jsii.String("Main"), &awsecs.ContainerDefinitionOptions{Image: image})
	app.Synth(nil)
	files, err := filepath.Glob(filepath.Join(out, "asset.*", "initial-config-*.base64"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	content, err := os.ReadFile(files[0])
	require.NoError(t, err)
	return files[0], string(content)
}

func TestGoBuild_StagesEmbeddedInitialConfig(t *testing.T) {
	cfg := &ports.BridgeConfig{Version: 41, Bridge: ports.BridgeSettings{ID: "embedded"}}
	file, encoded := stagedInitialConfig(t, cfg)
	data, err := parser.MarshalYAML(cfg)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(data), encoded)
	require.Equal(t, fmt.Sprintf("initial-config-%x.base64", sha256.Sum256(data)), filepath.Base(file))
	dockerfile, err := os.ReadFile(filepath.Join(filepath.Dir(file), "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "COPY "+filepath.Base(file))
	require.Contains(t, string(dockerfile), "initial-config.base64")
	require.NotContains(t, string(dockerfile), "GOENV")
	require.Contains(t, string(dockerfile), "/gobridge-filebased -initial-config-digest")
	require.NotContains(t, string(dockerfile), base64.StdEncoding.EncodeToString(data))
	require.Equal(t, 41, cfg.Version, "the build must not assign the target repository's version")

	cfg.Bridge.ID = "changed"
	changed, _ := stagedInitialConfig(t, cfg)
	require.NotEqual(t, filepath.Base(file), filepath.Base(changed), "Go's linker cache key must change with the payload")
}

func TestGoBuild_RejectsUnresolvedEmbeddedConfig(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("UnresolvedInitial"), nil)
	value := awscdk.NewCfnParameter(stack, jsii.String("BridgeName"), nil)
	require.PanicsWithValue(t, "gobridgecdk: embedded initial config contains unresolved CDK tokens; use stable names or selectors", func() {
		imgsource.NewGoBuild(imgsource.GoBuildProps{Version: "v0.4.0"}).
			Materialize(stack, jsii.String("Image"), &ports.BridgeConfig{
				Bridge: ports.BridgeSettings{ID: *value.ValueAsString()},
			})
	})
}

func TestGoBuild_RejectsLiteralQueueURLsInEmbeddedConfig(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("EmbeddedQueueURL"), nil)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "embedded"}}
	sender := ports.SenderDef{ID: "out", Transport: "sqs"}
	sender.SetDecoded(&sqs.Config{QueueURL: "https://sqs.eu-west-1.amazonaws.com/123456789012/orders"}, nil)
	cfg.Senders = append(cfg.Senders, sender)
	require.Panics(t, func() {
		imgsource.NewGoBuild(imgsource.GoBuildProps{Version: "v0.4.0"}).
			Materialize(stack, jsii.String("Image"), cfg)
	})
}

// TestGoBuild_NativeFileEmbedding verifies large payloads and content-sensitive
// caching with the real Go toolchain, without a module download or Docker.
func TestGoBuild_NativeFileEmbedding(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a local executable with the Go toolchain")
	}
	goTool, err := exec.LookPath("go")
	require.NoError(t, err)
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	program, err := os.ReadFile(filepath.Join("testdata", "initial_config_main.go"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(main, program, 0o600))
	placeholder := filepath.Join(dir, "initial-config.base64")
	require.NoError(t, os.WriteFile(placeholder, nil, 0o600))
	for _, id := range []string{"small", strings.Repeat("large", 40000)} {
		cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: id}}
		payload, _ := stagedInitialConfig(t, cfg)
		overlay := filepath.Join(dir, "overlay.json")
		mapping, err := json.Marshal(map[string]any{"Replace": map[string]string{placeholder: payload}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(overlay, mapping, 0o600))
		binary := filepath.Join(dir, "probe")
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		cmd := exec.CommandContext(ctx, goTool, "build", "-overlay="+overlay, "-o", binary, main)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOENV=off")
		output, buildErr := cmd.CombinedOutput()
		cancel()
		require.NoError(t, buildErr, "%s", output)
		ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
		output, runErr := exec.CommandContext(ctx, binary).Output()
		cancel()
		require.NoError(t, runErr)
		data, err := parser.MarshalYAML(cfg)
		require.NoError(t, err)
		require.Equal(t, base64.StdEncoding.EncodeToString(data), string(output))
	}
}
