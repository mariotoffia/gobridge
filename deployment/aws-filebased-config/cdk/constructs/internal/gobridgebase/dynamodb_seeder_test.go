//go:build !race

package gobridgebase_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// TestDynamoDBSeeder_AssetAndStartupGate pins source integrity, plugin projection and task wiring.
func TestDynamoDBSeeder_AssetAndStartupGate(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "asset", true: "inline"}[inline], func(t *testing.T) {
			stack, vpc, _ := newScope(t)
			src := source.NewAsset(writeTempYAML(t, sampleYAML+`version: 9007199254740993
stores:
  outbox:
    type: dynamodb
    options:
      table_name: test-outbox
`))
			mat, err := src.Materialize()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, mat.Close()) })
			if inline {
				src = source.NewInline(mat.Config)
			}
			expected, err := parser.MarshalBridgeConfigJSON(mat.Config)
			require.NoError(t, err)
			boot := bootstrap()
			boot.ConfigSource = infra.ConfigSourceDynamoDB
			boot.ConfigFilePath = ""
			b := gobridgebase.New(stack, jsii.String("DynamoBridge"), &gobridgebase.Props{
				Mode: gobridgebase.ModeControl, Vpc: vpc, Bootstrap: boot, Source: src,
				ConfigTable: gobridgebase.NewConfigTable(stack, boot),
				Image:       awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
			})
			require.NotNil(t, b.ConfigAsset)
			data, err := os.ReadFile(filepath.Join(*awscdk.Stage_Of(stack).Outdir(), *b.ConfigAsset.AssetPath()))
			require.NoError(t, err)
			assert.Equal(t, string(expected), string(data), "must use the parser's typed plugin projection without float conversion")
			assert.Contains(t, string(data), `"version":9007199254740993`)
			assert.Contains(t, string(data), `"table_name":"test-outbox"`)
			assert.NotContains(t, string(data), "${Token[")
			hash := sha256.Sum256(data)
			tpl := assertions.Template_FromStack(stack, nil)
			script, err := os.ReadFile(filepath.Join("..", "seeder", "seeder-ddb.sh"))
			require.NoError(t, err)
			tpl.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
				"ContainerDefinitions": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
					"Name": "seeder", "Essential": false,
					"EntryPoint": []any{"/bin/bash", "-c"}, "Command": []any{string(script)},
					"Environment": assertions.Match_ArrayWith(&[]any{map[string]any{"Name": "EXPECTED_HASH", "Value": hex.EncodeToString(hash[:])}}),
				})}),
			})
			policies, err := json.Marshal(tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil))
			require.NoError(t, err)
			assert.Contains(t, string(policies), "s3:GetObject")
			assert.Contains(t, string(policies), "dynamodb:PutItem")
		})
	}
}

// TestDynamoDBSeeder_ModeOverrides preserves explicit control and read-only worker policies.
func TestDynamoDBSeeder_ModeOverrides(t *testing.T) {
	for _, tc := range []struct {
		role gobridgebase.Mode
		mode string
	}{
		{gobridgebase.ModeControl, "Overwrite"},
		{gobridgebase.ModeControl, "AbortDeploy"},
		{gobridgebase.ModeWorker, "AbortDeploy"},
	} {
		t.Run(string(tc.role)+"/"+tc.mode, func(t *testing.T) {
			stack, vpc, _ := newScope(t)
			boot := bootstrap()
			boot.ConfigSource = infra.ConfigSourceDynamoDB
			gobridgebase.New(stack, jsii.String("Bridge"), &gobridgebase.Props{
				Mode: tc.role, Vpc: vpc, Bootstrap: boot,
				ConfigTable: gobridgebase.NewConfigTable(stack, boot),
				SeederMode:  jsii.String(tc.mode), WorkerSeederMode: jsii.String(tc.mode),
				Source: source.NewAsset(writeTempYAML(t, sampleYAML)),
				Image:  awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
			})
			assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
				"ContainerDefinitions": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
					"Name": "seeder", "Environment": assertions.Match_ArrayWith(&[]any{map[string]any{"Name": "MODE", "Value": tc.mode}}),
				})}),
			})
		})
	}
}

// TestDynamoDBSeeder_RejectsOversizeAsset fails at synth before uploading an unusable config.
func TestDynamoDBSeeder_RejectsOversizeAsset(t *testing.T) {
	stack, vpc, _ := newScope(t)
	boot := bootstrap()
	boot.ConfigSource = infra.ConfigSourceDynamoDB
	table := gobridgebase.NewConfigTable(stack, boot)
	require.PanicsWithValue(t, "gobridgebase: DynamoDB config asset exceeds the 399360-byte data limit", func() {
		gobridgebase.New(stack, jsii.String("OversizeBridge"), &gobridgebase.Props{
			Mode: gobridgebase.ModeControl, Vpc: vpc, Bootstrap: boot, ConfigTable: table,
			Source: source.NewAsset(writeTempYAML(t, "bridge:\n  id: "+strings.Repeat("x", 390*1024)+"\n")),
			Image:  awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		})
	})
}

// TestDynamoDBSeeder_RejectsWritableWorkerMode keeps the shared task role read-only.
func TestDynamoDBSeeder_RejectsWritableWorkerMode(t *testing.T) {
	for _, mode := range []string{"SeedOnce", "Overwrite", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			stack, vpc, _ := newScope(t)
			boot := bootstrap()
			boot.ConfigSource = infra.ConfigSourceDynamoDB
			table := gobridgebase.NewConfigTable(stack, boot)
			require.PanicsWithValue(t, "gobridgebase: DynamoDB worker seeder mode must be AdoptValid or AbortDeploy", func() {
				gobridgebase.New(stack, jsii.String("Worker"), &gobridgebase.Props{
					Mode: gobridgebase.ModeWorker, Vpc: vpc, Bootstrap: boot,
					ConfigTable: table, WorkerSeederMode: jsii.String(mode),
					Source: source.NewAsset(writeTempYAML(t, sampleYAML)),
					Image:  awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
				})
			})
		})
	}
}

// TestDynamoDBSeeder_RejectsUnresolvedAssetToken prevents literal tokens reaching the runtime.
func TestDynamoDBSeeder_RejectsUnresolvedAssetToken(t *testing.T) {
	stack, vpc, _ := newScope(t)
	boot := bootstrap()
	boot.ConfigSource = infra.ConfigSourceDynamoDB
	table := gobridgebase.NewConfigTable(stack, boot)
	token := *awscdk.Token_AsString(awscdk.Fn_ImportValue(jsii.String("BridgeID")), nil)
	require.PanicsWithValue(t, "gobridgebase: DynamoDB config asset contains unresolved CDK tokens; use resolved physical resource names", func() {
		gobridgebase.New(stack, jsii.String("TokenBridge"), &gobridgebase.Props{
			Mode: gobridgebase.ModeControl, Vpc: vpc, Bootstrap: boot, ConfigTable: table,
			Source: source.NewAsset(writeTempYAML(t, "bridge:\n  id: '"+token+"'\n")),
			Image:  awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		})
	})
}
