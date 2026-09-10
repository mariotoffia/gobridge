package imgsource_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecr"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/stretchr/testify/require"
)

func TestRegistry_MaterializePreservesReference(t *testing.T) {
	ref := "registry.example.com/bridge:v1.2.3@sha256:" + strings.Repeat("a", 64)
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Registry"), nil)
	task := awsecs.NewFargateTaskDefinition(stack, jsii.String("Task"), nil)
	task.AddContainer(jsii.String("Bridge"), &awsecs.ContainerDefinitionOptions{
		Image: imgsource.NewRegistry(ref).Materialize(stack, jsii.String("Image"), nil),
	})
	assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"ContainerDefinitions": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{"Image": ref})}),
	})
}

func TestRegistry_RejectsUnpinnedOrUnsafeReferences(t *testing.T) {
	for _, ref := range []string{
		"", "bridge:latest", "bridge@sha256:abc", "bridge@sha256:" + strings.Repeat("0", 64),
		"bridge;false@sha256:" + strings.Repeat("a", 64),
		"bridge\n@sha256:" + strings.Repeat("a", 64),
	} {
		t.Run(ref, func(t *testing.T) {
			require.Panics(t, func() { imgsource.NewRegistry(ref).Materialize(nil, nil, nil) })
		})
	}
}

func TestEcr_MaterializePreservesRepositoryTagAndPullGrant(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Ecr"), nil)
	repo := awsecr.NewRepository(stack, jsii.String("Repo"), nil)
	task := awsecs.NewFargateTaskDefinition(stack, jsii.String("Task"), nil)
	task.AddContainer(jsii.String("Bridge"), &awsecs.ContainerDefinitionOptions{
		Image: imgsource.NewEcrRepository(repo, "v1.2.3").Materialize(stack, jsii.String("Image"), nil),
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"ContainerDefinitions": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
			"Image": stack.Resolve(repo.RepositoryUriForTag(jsii.String("v1.2.3"))),
		})}),
	})
	tpl.HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
		"PolicyDocument": assertions.Match_ObjectLike(&map[string]any{
			"Statement": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
				"Action":   assertions.Match_ArrayWith(&[]any{"ecr:BatchGetImage"}),
				"Resource": stack.Resolve(repo.RepositoryArn()),
			})}),
		}),
	})
	require.Panics(t, func() { imgsource.NewEcrRepository(nil, "v1").Materialize(nil, nil, nil) })
	require.Panics(t, func() { imgsource.NewEcrRepository(repo, "").Materialize(stack, jsii.String("Empty"), nil) })
}

func TestGoBuild_MaterializeStagesStableAssetAndCleansTemporaryContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platform   string
		stagingOff bool
	}{
		{name: "default"},
		{name: "amd64", platform: "linux/amd64"},
		{name: "arm64", platform: "linux/arm64"},
		{name: "amd64 staging disabled", platform: "linux/amd64", stagingOff: true},
		{name: "arm64 staging disabled", platform: "linux/arm64", stagingOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			out := t.TempDir()
			appProps := &awscdk.AppProps{Outdir: jsii.String(out)}
			if tc.stagingOff {
				appProps.Context = &map[string]any{"aws:cdk:asset-staging": false}
			}
			app := awscdk.NewApp(appProps)
			stack := awscdk.NewStack(app, jsii.String("Build"), nil)
			tags := []string{"custom"}
			src := imgsource.NewGoBuild(imgsource.GoBuildProps{Version: "v0.4.0", Platform: tc.platform, BuildTags: tags})
			tags[0] = "mutated"
			task := awsecs.NewFargateTaskDefinition(stack, jsii.String("Task"), &awsecs.FargateTaskDefinitionProps{
				RuntimePlatform: src.RuntimePlatform(),
			})
			for _, id := range []string{"First", "Second"} {
				task.AddContainer(jsii.String(id), &awsecs.ContainerDefinitionOptions{
					Image: src.Materialize(stack, jsii.String(id+"Image"), nil),
				})
			}
			app.Synth(nil)
			leftovers, err := filepath.Glob(filepath.Join(temp, "gobridgecdk-image-*"))
			require.NoError(t, err)
			require.Empty(t, leftovers)
			files, err := filepath.Glob(filepath.Join(out, "asset.*", "Dockerfile"))
			require.NoError(t, err)
			require.Len(t, files, 1, "identical builds must share one staged asset")
			content, err := os.ReadFile(files[0])
			require.NoError(t, err)
			require.Contains(t, string(content), "-tags=custom ")
			manifest, err := os.ReadFile(filepath.Join(out, "Build.assets.json"))
			require.NoError(t, err)
			var assets struct {
				DockerImages map[string]struct {
					Source struct{ Directory, Platform string }
				}
			}
			require.NoError(t, json.Unmarshal(manifest, &assets))
			require.Len(t, assets.DockerImages, 1)
			platform := tc.platform
			if platform == "" {
				platform = "linux/amd64"
			}
			for _, asset := range assets.DockerImages {
				require.Equal(t, platform, asset.Source.Platform)
				require.FileExists(t, filepath.Join(out, asset.Source.Directory, "Dockerfile"))
				require.False(t, filepath.IsAbs(asset.Source.Directory), "assembly must be portable")
			}
			if tc.stagingOff {
				require.Equal(t, false, app.Node().TryGetContext(jsii.String("aws:cdk:asset-staging")),
					"forcing image staging must not change the app's context")
			}
			arch := "X86_64"
			if platform == "linux/arm64" {
				arch = "ARM64"
			}
			assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
				"RuntimePlatform": map[string]any{"CpuArchitecture": arch, "OperatingSystemFamily": "LINUX"},
			})
		})
	}
}

func TestEcr_MaterializePreservesDigest(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("EcrDigest"), nil)
	repo := awsecr.NewRepository(stack, jsii.String("Repo"), nil)
	digest := "sha256:" + strings.Repeat("b", 64)
	task := awsecs.NewFargateTaskDefinition(stack, jsii.String("Task"), nil)
	task.AddContainer(jsii.String("Bridge"), &awsecs.ContainerDefinitionOptions{
		Image: imgsource.NewEcrRepository(repo, digest).Materialize(stack, jsii.String("Image"), nil),
	})
	assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"ContainerDefinitions": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
			"Image": stack.Resolve(repo.RepositoryUriForDigest(jsii.String(digest))),
		})}),
	})
}
