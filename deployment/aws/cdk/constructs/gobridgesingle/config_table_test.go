//go:build !race

package gobridgesingle_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/imgsource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

func configSingleStack(t *testing.T, boot infra.BootstrapConfig, src source.Source) (awscdk.Stack, *gobridgesingle.GoBridgeSingle) {
	t.Helper()
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("ConfigStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	g := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc: vpc, Image: imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap: boot, BridgeConfig: src,
	})
	return stack, g
}

func dynamoBootstrap() infra.BootstrapConfig {
	boot := singleBootstrap()
	boot.ConfigSource = infra.ConfigSourceDynamoDB
	boot.ConfigFilePath = ""
	return boot
}

// TestSingle_ConfigTable_RetainedOnDemand verifies the config table survives replacement and deletion.
func TestSingle_ConfigTable_RetainedOnDemand(t *testing.T) {
	stack, _ := configSingleStack(t, dynamoBootstrap(), source.NewAsset(writeSingleYAML(t, singleSampleYAML)))
	tpl := assertions.Template_FromStack(stack, nil)
	tables := tpl.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
	require.Len(t, *tables, 1, "DynamoDB config needs exactly one owned table")
	tpl.HasResource(jsii.String("AWS::DynamoDB::Table"), map[string]any{
		"DeletionPolicy": "Retain", "UpdateReplacePolicy": "Retain",
		"Properties": map[string]any{
			"BillingMode":                      "PAY_PER_REQUEST",
			"KeySchema":                        []any{map[string]any{"AttributeName": "PK", "KeyType": "HASH"}, map[string]any{"AttributeName": "SK", "KeyType": "RANGE"}},
			"AttributeDefinitions":             []any{map[string]any{"AttributeName": "PK", "AttributeType": "S"}, map[string]any{"AttributeName": "SK", "AttributeType": "S"}},
			"PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true},
			"TimeToLiveSpecification":          assertions.Match_Absent(),
			"StreamSpecification":              assertions.Match_Absent(),
		},
	})
}

// TestSingle_ConfigTable_BootstrapCopy verifies settings are copied before stamping the owned table name.
func TestSingle_ConfigTable_BootstrapCopy(t *testing.T) {
	for _, watch := range []string{"", "poll", "streams"} {
		t.Run("watch="+watch, func(t *testing.T) {
			settings := &infra.ConfigDynamoDBSettings{TableName: "caller-table", WatchMode: watch, StreamPollInterval: "2s"}
			before := *settings
			boot := dynamoBootstrap()
			boot.ConfigDynamoDB = settings
			stack, _ := configSingleStack(t, boot, source.NewAsset(writeSingleYAML(t, singleSampleYAML)))
			tpl := assertions.Template_FromStack(stack, nil)
			tables := tpl.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
			require.Len(t, *tables, 1)
			var tableID string
			for id := range *tables {
				tableID = id
			}
			tasks := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
			require.Len(t, *tasks, 1)
			for _, task := range *tasks {
				got := singleTaskBootstrap(t, (*task)["Properties"].(map[string]any))
				require.NotNil(t, got.ConfigDynamoDB)
				assert.Equal(t, "Ref:"+tableID, got.ConfigDynamoDB.TableName)
				assert.Equal(t, infra.ConfigSourceDynamoDB, got.ConfigSource)
				assert.Empty(t, got.ConfigFilePath)
				expectedWatch := watch
				if expectedWatch == "" {
					expectedWatch = "poll"
				}
				assert.Equal(t, expectedWatch, got.ConfigDynamoDB.WatchMode)
				assert.Equal(t, "2s", got.ConfigDynamoDB.StreamPollInterval)
			}
			assert.Same(t, settings, boot.ConfigDynamoDB)
			assert.Equal(t, before, *settings)
		})
	}
}

// singleTaskBootstrap evaluates only the string join and table Ref emitted in the environment.
// Using the logical ID as a stand-in lets assertions detect a literal caller table or a wrong Ref.
func singleTaskBootstrap(t *testing.T, task map[string]any) infra.BootstrapConfig {
	t.Helper()
	for _, raw := range task["ContainerDefinitions"].([]any) {
		container := raw.(map[string]any)
		if container["Name"] != "gobridge" {
			continue
		}
		for _, rawEnv := range container["Environment"].([]any) {
			env := rawEnv.(map[string]any)
			if env["Name"] != "GOBRIDGE_AWS_BOOTSTRAP_JSON" {
				continue
			}
			value, ok := env["Value"].(string)
			if !ok {
				join := env["Value"].(map[string]any)["Fn::Join"].([]any)
				require.Equal(t, "", join[0])
				var parts []string
				for _, part := range join[1].([]any) {
					if text, ok := part.(string); ok {
						parts = append(parts, text)
					} else {
						parts = append(parts, "Ref:"+part.(map[string]any)["Ref"].(string))
					}
				}
				value = strings.Join(parts, "")
			}
			var boot infra.BootstrapConfig
			require.NoError(t, json.Unmarshal([]byte(value), &boot))
			return boot
		}
	}
	t.Fatal("bootstrap environment is missing")
	return infra.BootstrapConfig{}
}
