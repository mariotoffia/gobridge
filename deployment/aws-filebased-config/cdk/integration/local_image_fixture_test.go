//go:build integration_local && !race

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

func TestLocalFixtures_EmbedConfigWithoutConfigPublication(t *testing.T) {
	registry := ports.NewRegistry()
	require.NoError(t, awsstore.Register(registry))
	require.NoError(t, sqsadapter.Register(registry))
	require.NoError(t, httptransport.Register(registry))
	require.NoError(t, paho.Register(registry))
	require.NoError(t, nativestore.Register(registry))
	env := SandboxEnv{
		Account: localAccount, Region: localRegion, VpcID: "vpc-12345678",
		AvailabilityZones: []string{"us-east-1a", "us-east-1b"},
		SubnetIDs:         []string{"subnet-12345678", "subnet-23456789"},
		PublicSubnetIDs:   []string{"subnet-34567890", "subnet-45678901"},
	}
	for _, tc := range []struct {
		name      string
		taskCount int
		build     func(*testing.T, awscdk.Stack)
	}{
		{"SQS", 1, func(_ *testing.T, stack awscdk.Stack) { newLocalSQSFixture(stack, env, "sqs") }},
		{"MQTT", 1, func(_ *testing.T, stack awscdk.Stack) { newLocalMQTTFixture(stack, env, "mqtt") }},
		{"Cluster", 2, func(_ *testing.T, stack awscdk.Stack) { newLocalClusterFixture(stack, env, "cluster", 2) }},
		{"HAFile", 3, func(t *testing.T, stack awscdk.Stack) { localImageHAFixture(t, stack, env, infra.ConfigSourceFile) }},
		{"HADynamoDB", 3, func(t *testing.T, stack awscdk.Stack) {
			localImageHAFixture(t, stack, env, infra.ConfigSourceDynamoDB)
		}},
		{"Lambda", 1, func(t *testing.T, stack awscdk.Stack) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap"), []byte("fixture binary; synth only"), 0o700))
			newLocalLambdaFixture(stack, env, "lambda", dir, awslambda.Architecture_X86_64())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
			stack := awscdk.NewStack(app, jsii.String(tc.name), &awscdk.StackProps{Env: StackEnv(env)})
			tc.build(t, stack)
			app.Synth(nil)
			assets, err := readLocalImageAssets(*app.Outdir(), tc.name)
			require.NoError(t, err)
			data, err := os.ReadFile(filepath.Join(*app.Outdir(), tc.name+".template.json"))
			require.NoError(t, err)
			var template struct{ Resources map[string]map[string]any }
			require.NoError(t, json.Unmarshal(data, &template))
			tasks := 0
			for _, resource := range template.Resources {
				if resource["Type"] != taskDefinitionType {
					continue
				}
				tasks++
				properties := resource["Properties"].(map[string]any)
				require.Len(t, properties["ContainerDefinitions"], 1, "configuration initialization runs inside the runtime")
				container, err := runtimeContainer(properties)
				require.NoError(t, err)
				asset, err := assets.runtimeAsset(container["Image"])
				require.NoError(t, err, "image=%v assets=%v", container["Image"], assets.manifest["dockerImages"])
				require.Equal(t, "linux/amd64", asset.Platform)
				cfg, err := parser.Parse(bytes.NewReader(asset.Config), parser.FormatYAML, registry)
				require.NoError(t, err)
				require.NotEmpty(t, cfg.Bridge.ID)
				require.NotEmpty(t, cfg.Routes)
				require.NotContains(t, string(asset.Config), "${Token")
				require.NotContains(t, container, "DependsOn", "no configuration sidecar may gate the runtime")
				bindVolumesToHost(t, tc.name, properties, filepath.Join(*app.Outdir(), "config-mount"))
				_, storage, err := declaredTaskSpec(properties)
				require.NoError(t, err)
				if tc.name == "HADynamoDB" {
					require.Empty(t, storage.Volumes)
					require.Empty(t, storage.Mounts)
				} else {
					require.NotEmpty(t, storage.Volumes)
					require.Len(t, storage.Mounts, 1)
				}
				if tc.name == "HAFile" || tc.name == "HADynamoDB" {
					requireEmbeddedHABaseline(t, container, cfg, tc.name == "HADynamoDB")
				}
			}
			require.Equal(t, tc.taskCount, tasks)
			// File assets still serve Lambda/custom resources and the template,
			// but the config itself exists only inside the runtime build asset.
			for _, raw := range assets.manifest["files"].(map[string]any) {
				entry := raw.(map[string]any)
				source := entry["source"].(map[string]any)
				path, _ := source["path"].(string)
				require.NotEqual(t, ".yaml", filepath.Ext(path))
				require.NotEqual(t, ".yml", filepath.Ext(path))
			}
			require.NoError(t, assets.save())
			require.Empty(t, assets.manifest["dockerImages"], "the dummy module version must never be built or published")
		})
	}
}

func requireEmbeddedHABaseline(t *testing.T, container map[string]any, cfg *ports.BridgeConfig, dynamodb bool) {
	t.Helper()
	var boot infra.BootstrapConfig
	found := false
	for _, raw := range asList(container["Environment"]) {
		pair := raw.(map[string]any)
		if pair["Name"] != bootstrapDocumentVariable {
			continue
		}
		found = true
		if dynamodb {
			var err error
			boot, err = volumeFreeBootstrap(pair["Value"])
			require.NoError(t, err)
		} else {
			text, ok := pair["Value"].(string)
			require.True(t, ok, "file-backed HA bootstrap must contain no unresolved values")
			require.NoError(t, json.Unmarshal([]byte(text), &boot))
		}
	}
	require.True(t, found)
	digest, err := bridge.DeploymentBaselineContentDigest(cfg)
	require.NoError(t, err)
	require.Equal(t, digest, boot.DynamoDBHABaselineConfigDigest)
	cfg.Version = 1
	digest, err = bridge.DeploymentBaselineContentDigest(cfg)
	require.NoError(t, err)
	require.Equal(t, digest, boot.DynamoDBHABaselineConfigDigest, "target-owned version 1 must preserve the deployment baseline")
	require.Equal(t, bridge.DeploymentProfileFingerprint(cfg), boot.DynamoDBHAConfigFingerprint)
}

func localImageHAFixture(t *testing.T, stack awscdk.Stack, env SandboxEnv, source string) {
	t.Helper()
	_ = newHAFixture(t, stack, haSandbox{
		SandboxEnv: env, Image: localRuntimeImageSource(stack),
		BrokerURL: "tcp://mosquitto:1883", MQTTClientID: "gobridge-local-ha",
		MQTTCredentialParam: localMQTTParam, AdminParam: localAdminParam, ProbeCIDR: "10.0.0.0/8",
		PlaintextBroker: true, ConfigSource: source,
	}, staticSlotRoster())
}
