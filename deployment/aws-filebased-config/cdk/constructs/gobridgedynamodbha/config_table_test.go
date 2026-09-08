//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// TestDynamoDBHA_ConfigTable_SharedGrants verifies every task uses one table with role-specific access.
// Control writes config; workers only read it. Streams add read access only to that stream.
func TestDynamoDBHA_ConfigTable_SharedGrants(t *testing.T) {
	for _, tc := range []struct {
		name, watch string
		slots       bool
	}{
		{name: "poll"}, {name: "streams", watch: "streams"}, {name: "static slots", watch: "streams", slots: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &infra.ConfigDynamoDBSettings{TableName: "caller-table", WatchMode: tc.watch, StreamPollInterval: "2s"}
			before := *settings
			mutate := func(p *ha.DynamoDBHAProps) {
				p.Bootstrap.ConfigSource = infra.ConfigSourceDynamoDB
				p.Bootstrap.ConfigFilePath = ""
				p.Bootstrap.ConfigDynamoDB = settings
			}
			var h *haHarness
			if tc.slots {
				h = newStaticSlotHarness(t, mutate)
			} else {
				h = newHAHarness(t, mutate)
			}
			tpl := assertions.Template_FromStack(h.stack, nil)
			tables := tpl.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
			wantTables, wantTasks := 4, 2
			if tc.slots {
				wantTables, wantTasks = 5, 3
			}
			require.Len(t, *tables, wantTables, "one config table in addition to the unchanged data tables")
			var configID string
			for id, raw := range *tables {
				if strings.Contains(id, "ConfigTable") {
					require.Empty(t, configID, "must not create a config table per base")
					configID = id
					props := (*raw)["Properties"].(map[string]any)
					if tc.watch == "streams" {
						assert.Equal(t, map[string]any{"StreamViewType": "KEYS_ONLY"}, props["StreamSpecification"])
					} else {
						assert.NotContains(t, props, "StreamSpecification")
					}
				}
			}
			require.NotEmpty(t, configID)
			tasks := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
			require.Len(t, *tasks, wantTasks)
			for id, raw := range *tasks {
				main := mainContainerFromTask(t, *raw)
				boot := haConfigBootstrap(t, main)
				assert.Equal(t, []any{map[string]any{"ContainerName": "seeder", "Condition": "SUCCESS"}}, main["DependsOn"])
				containers := (*raw)["Properties"].(map[string]any)["ContainerDefinitions"].([]any)
				require.Len(t, containers, 2)
				for _, entry := range containers {
					container := entry.(map[string]any)
					if container["Name"] != "seeder" {
						continue
					}
					env := map[string]any{}
					for _, raw := range container["Environment"].([]any) {
						value := raw.(map[string]any)
						env[value["Name"].(string)] = value["Value"]
					}
					mode := "AdoptValid"
					if boot.NodeRole == infra.NodeRoleControl {
						mode = "SeedOnce"
					}
					assert.Equal(t, mode, env["MODE"])
					assert.Equal(t, map[string]any{"Ref": configID}, env["TABLE"])
					assert.Equal(t, "config#"+boot.BridgeID, env["PK"])
					assert.NotEmpty(t, env["ITEM_S3_URI"])
					assert.Regexp(t, "^[0-9a-f]{64}$", env["EXPECTED_HASH"])
					assert.NotContains(t, env, "EFS_TARGET_PATH")
					assert.Equal(t, false, container["Essential"])
				}
				require.NotNil(t, boot.ConfigDynamoDB)
				assert.Equal(t, "Ref:"+configID, boot.ConfigDynamoDB.TableName, id)
				assert.Equal(t, "2s", boot.ConfigDynamoDB.StreamPollInterval)
				roleID := (*raw)["Properties"].(map[string]any)["TaskRoleArn"].(map[string]any)["Fn::GetAtt"].([]any)[0].(string)
				assertConfigAssetRead(t, tpl, roleID)
				tableActions, streamActions := configRoleActions(t, tpl, roleID, configID)
				for _, action := range []string{"dynamodb:GetItem", "dynamodb:Query", "dynamodb:Scan"} {
					assert.True(t, tableActions[action], "%s lacks %s", id, action)
				}
				for _, action := range []string{"dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:BatchWriteItem"} {
					assert.Equal(t, boot.NodeRole == infra.NodeRoleControl, tableActions[action], "%s %s", id, action)
				}
				assert.False(t, tableActions["dynamodb:*"], "config grants must not grant all DynamoDB actions")
				for _, action := range []string{"dynamodb:DescribeStream", "dynamodb:GetRecords", "dynamodb:GetShardIterator"} {
					assert.Equal(t, tc.watch == "streams", streamActions[action], "%s %s", id, action)
				}
				if tc.watch == "streams" {
					assert.True(t, streamActions["dynamodb:ListStreams"])
				}
			}
			assert.Equal(t, before, *settings, "facade must not mutate caller-owned settings")
		})
	}
}

func assertConfigAssetRead(t *testing.T, tpl assertions.Template, roleID string) {
	t.Helper()
	for _, raw := range *tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		roles, err := json.Marshal(props["Roles"])
		require.NoError(t, err)
		if !strings.Contains(string(roles), `"`+roleID+`"`) {
			continue
		}
		for _, rawStatement := range props["PolicyDocument"].(map[string]any)["Statement"].([]any) {
			statement := rawStatement.(map[string]any)
			actions, err := json.Marshal(statement["Action"])
			require.NoError(t, err)
			if statement["Effect"] == "Allow" && strings.Contains(string(actions), "s3:GetObject") {
				resource, err := json.Marshal(statement["Resource"])
				require.NoError(t, err)
				assert.Contains(t, string(resource), "cdk-hnb659fds-assets-")
				assert.NotEqual(t, `"*"`, string(resource), "asset read must be bucket-scoped")
				assert.NotContains(t, string(actions), "s3:PutObject")
				return
			}
		}
	}
	t.Errorf("task role %s has no config asset read grant", roleID)
}

func configRoleActions(t *testing.T, tpl assertions.Template, roleID, tableID string) (map[string]bool, map[string]bool) {
	t.Helper()
	tableActions, streamActions := map[string]bool{}, map[string]bool{}
	for _, raw := range *tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		roles, err := json.Marshal(props["Roles"])
		require.NoError(t, err)
		if !strings.Contains(string(roles), `"`+roleID+`"`) {
			continue
		}
		for _, rawStatement := range props["PolicyDocument"].(map[string]any)["Statement"].([]any) {
			statement := rawStatement.(map[string]any)
			if statement["Effect"] != "Allow" {
				continue
			}
			resources, err := json.Marshal(statement["Resource"])
			require.NoError(t, err)
			wildcard := string(resources) == `"*"`
			if !wildcard && !strings.Contains(string(resources), `"`+tableID+`"`) {
				continue
			}
			actions, ok := statement["Action"].([]any)
			if !ok {
				actions = []any{statement["Action"]}
			}
			for _, rawAction := range actions {
				action := rawAction.(string)
				if wildcard || strings.Contains(string(resources), "StreamArn") {
					streamActions[action] = true
				}
				if wildcard || !strings.Contains(string(resources), "StreamArn") {
					tableActions[action] = true
				}
			}
		}
	}
	return tableActions, streamActions
}

func haConfigBootstrap(t *testing.T, main map[string]any) infra.BootstrapConfig {
	t.Helper()
	for _, rawEnv := range main["Environment"].([]any) {
		env := rawEnv.(map[string]any)
		if env["Name"] != "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON" {
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
	t.Fatal("bootstrap environment is missing")
	return infra.BootstrapConfig{}
}
