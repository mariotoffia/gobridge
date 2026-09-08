//go:build integration_local

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/stretchr/testify/require"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// The shipped seeder must establish the initial config, with no test seed or
// config file. Subsequent CAS writes go straight to that table, not through the
// admin apply path, so only the deployed source watcher can initiate a rollout.
//
//	SeedOnce -> source table v1 -> CAS v2 (debug) -> CAS v3 (original)
//	                  |                  |                 |
//	                  +--------- watcher/coordinator -----+
//	                                     |
//	                     applied-config reads on all three members
func TestLocal_DynamoDBConfigHotReload(t *testing.T) {
	env := RequireSandbox(t)
	cohort := deployLocalCohort(t, env, staticSlotRoster(), infra.ConfigSourceDynamoDB)
	t.Cleanup(func() {
		if t.Failed() {
			cohort.LogContainers(t, context.Background())
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 55*time.Minute)
	defer cancel()
	roster := haMemberIDs(staticSlotRoster())
	require.ElementsMatch(t, roster, strings.Split(cohort.Outputs["MemberSlotIDs"], ","))
	boot := requireDynamoDBConfigDeployment(t, ctx, cohort)
	client := dynamodb.NewFromConfig(localAWSConfig(t))
	reg := ports.NewRegistry()
	require.NoError(t, paho.Register(reg))
	require.NoError(t, sqsadapter.Register(reg))
	require.NoError(t, awsstore.Register(reg))
	store := ddbconfig.NewLoader(client, ddbconfig.WithTableName(boot.ConfigDynamoDB.TableName),
		ddbconfig.WithBridgeID(boot.BridgeID), ddbconfig.WithRegistry(reg))

	probe := cohort.Probe()
	slots := waitForEverySlot(t, ctx, probe, cohort.AdminKey, roster)
	initial := readDynamoDBConfig(t, ctx, client, store, boot)
	require.Equal(t, 1, initial.Version, "only the shipped SeedOnce seeder may create the initial item")
	requireDynamoDBSeederSucceeded(t, cohort)
	digest, err := bridge.ConfigArtifactDigest(initial)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	content, err := bridge.DeploymentBaselineContentDigest(initial)
	require.NoError(t, err)
	require.Equal(t, boot.DynamoDBHABaselineConfigDigest, content)
	require.Equal(t, boot.DynamoDBHAConfigFingerprint, bridge.DeploymentProfileFingerprint(initial))
	for id, health := range slots {
		require.Zero(t, health.Generation, id)
		require.Zero(t, health.BaselineGeneration, id)
		require.Equal(t, digest, health.BaselineDigest, id)
		// A standby's last call can report a held coordinator lease. Freshness,
		// not an empty last-call error, determines whether this observation counts.
		require.True(t, health.fresh(), id)
	}
	requireCohortConfig(t, ctx, cohort, roster, initial)
	t.Logf("all %d slots share generation-zero baseline %s, seeded source version %d", len(slots), digest, initial.Version)

	originalLevel := initial.Bridge.LogLevel
	require.NotEqual(t, "debug", originalLevel)
	generation := uint64(0)
	for _, level := range []string{"debug", originalLevel} {
		current := readDynamoDBConfig(t, ctx, client, store, boot)
		previousVersion := current.Version
		current.Bridge.LogLevel = level
		require.Equal(t, boot.DynamoDBHAConfigFingerprint, bridge.DeploymentProfileFingerprint(current),
			"hot reload must not change the immutable deployment profile")
		require.NoError(t, store.SaveIfVersion(ctx, current, previousVersion))
		require.Equal(t, previousVersion+1, current.Version)
		// A stale writer must lose against the reference DynamoDB implementation.
		require.ErrorIs(t, store.SaveIfVersion(ctx, current, previousVersion), shared.ErrVersionMismatch)
		written := readDynamoDBConfig(t, ctx, client, store, boot)
		want, err := parser.MarshalBridgeConfigJSON(current)
		require.NoError(t, err)
		got, err := parser.MarshalBridgeConfigJSON(written)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got), "typed plugin options must survive the table write")
		generation = waitCohortApplied(t, ctx, probe, cohort.AdminKey, roster, generation)
		waitForEverySlot(t, ctx, probe, cohort.AdminKey, roster)
		requireCohortConfig(t, ctx, cohort, roster, written)
		t.Logf("table version %d round-tripped log_level=%q on every slot; settled generation %d",
			written.Version, level, generation)
	}
	entries, err := os.ReadDir(cohort.ConfigDir)
	require.NoError(t, err)
	require.Empty(t, entries, "DynamoDB-only tasks must never write a shared config file")
}

func readDynamoDBConfig(t *testing.T, ctx context.Context, client *dynamodb.Client, store *ddbconfig.Loader,
	boot infra.BootstrapConfig,
) *ports.BridgeConfig {
	t.Helper()
	row, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(boot.ConfigDynamoDB.TableName), ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: "config#" + boot.BridgeID},
			"SK": &ddbtypes.AttributeValueMemberS{Value: "current"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, row.Item, "the deployed seeder must write the initial item, not this test")
	version, ok := row.Item["version"].(*ddbtypes.AttributeValueMemberN)
	require.True(t, ok, "config row must carry a numeric version")
	data, ok := row.Item["data"].(*ddbtypes.AttributeValueMemberS)
	require.True(t, ok, "config row must carry JSON data")
	var document struct{ Version int }
	require.NoError(t, json.Unmarshal([]byte(data.Value), &document))
	require.Equal(t, strconv.Itoa(document.Version), version.Value, "row and JSON versions must agree")
	cfg, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, document.Version, cfg.Version)
	return cfg
}

// GET /admin/config reads appliedRef, not the table or the last desired config.
// This is the content assertion a generation-only rollout check cannot supply.
func requireCohortConfig(t *testing.T, ctx context.Context, cohort LocalCohort, roster []string, want *ports.BridgeConfig) {
	t.Helper()
	for _, id := range roster {
		host := cohort.MemberHost(t, ctx, id)
		var applied struct{ Config *ports.BridgeConfig }
		adminCall(t, ctx, cohort.Probe(), http.MethodGet,
			slotURL(host, slotAdminPort, "/api/v1/admin/config"), cohort.AdminKey, nil, &applied)
		require.NotNil(t, applied.Config, id)
		require.Equal(t, want.Version, applied.Config.Version, id)
		require.Equal(t, want.Bridge.LogLevel, applied.Config.Bridge.LogLevel, id)
		require.Equal(t, want.Bridge.ID, applied.Config.Bridge.ID, id)
		require.Equal(t, want.Routes, applied.Config.Routes, id)
		health, err := cohort.DeepHealth(ctx, host)
		require.NoError(t, err)
		require.True(t, health.Running, id)
		require.False(t, health.Empty, id)
		require.False(t, health.ConfigWatch.Degraded, "%s: %+v", id, health.ConfigWatch)
		require.Empty(t, health.ConfigWatch.LastApplyError, id)
	}
}

func requireDynamoDBConfigDeployment(t *testing.T, ctx context.Context, cohort LocalCohort) infra.BootstrapConfig {
	t.Helper()
	requireMirroredTable(t, cohort.Outputs["ConfigTableName"])
	declared := 0
	for id, raw := range synthesizedResources(t, cohort.LocalStack) {
		resource := raw.(map[string]any)
		kind := resource["Type"].(string)
		require.False(t, strings.HasPrefix(kind, "AWS::EFS::"), "unexpected filesystem resource %s", id)
		if kind == taskDefinitionType {
			_, spec, err := declaredTaskSpec(resource["Properties"].(map[string]any))
			require.NoError(t, err)
			require.Empty(t, spec.Volumes, id)
			require.Empty(t, spec.Mounts, id)
			declared++
		}
	}
	require.Equal(t, 3, declared)
	var control infra.BootstrapConfig
	for service, original := range cohort.backend.deployedTaskDefs {
		services, err := cohort.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster: aws.String(cohort.clusterARN), Services: []string{service},
		})
		require.NoError(t, err)
		require.Len(t, services.Services, 1)
		require.Equal(t, original, aws.ToString(services.Services[0].TaskDefinition), "volume-free tasks need no re-registration")
		definition, err := cohort.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(original)})
		require.NoError(t, err)
		require.NoError(t, verifyVolumeFreeTask(definition.TaskDefinition))
		for _, container := range definition.TaskDefinition.ContainerDefinitions {
			for _, pair := range container.Environment {
				if aws.ToString(pair.Name) != bootstrapDocumentVariable {
					continue
				}
				boot, err := volumeFreeBootstrap(aws.ToString(pair.Value))
				require.NoError(t, err)
				require.Equal(t, cohort.Outputs["ConfigTableName"], boot.ConfigDynamoDB.TableName)
				require.Equal(t, "poll", boot.ConfigDynamoDB.WatchMode)
				require.False(t, boot.DevMode, "the runtime must not create or seed the table")
				if boot.NodeRole == infra.NodeRoleControl {
					control = boot
				}
			}
		}
	}
	require.NotEmpty(t, control.BridgeID)
	return control
}

func requireDynamoDBSeederSucceeded(t *testing.T, cohort LocalCohort) {
	t.Helper()
	tasks, err := cohort.runningTasks(t.Context())
	require.NoError(t, err)
	require.Len(t, tasks, 3)
	for _, task := range tasks {
		taskID := task.arn[strings.LastIndex(task.arn, "/")+1:]
		containers := taskContainers(taskID)
		require.NotEmpty(t, containers, task.arn)
		for _, name := range containers {
			mounts, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect", "--format", "{{json .Mounts}}", name)
			require.NoError(t, err)
			require.JSONEq(t, "[]", strings.TrimSpace(string(mounts)), name)
		}
	}
	// A task can start before its table is mirrored or its seeder finishes. The
	// emulator deletes that task's containers when replacing it, including the
	// successful seeder. Its own log stream retains their stdout; match only the
	// task families this deployment declared, not an earlier stack's seed.
	logs, err := dockerexec.Run(dockerexec.LogsTimeout, "logs", flocilocal.ContainerName(t))
	require.NoError(t, err)
	for _, line := range strings.Split(string(logs), "\n") {
		for family := range cohort.backend.taskSpecs {
			_, payload, found := strings.Cut(line, "[ecs:"+family+":seeder] ")
			if !found {
				continue
			}
			var result struct {
				Mode, Reason string
				Exit         *int
			}
			if json.Unmarshal([]byte(payload), &result) == nil && result.Mode == "SeedOnce" && result.Reason == "seeded" && result.Exit != nil && *result.Exit == 0 {
				t.Logf("shipped DynamoDB seeder succeeded for %s: %s", family, payload)
				return
			}
		}
	}
	t.Fatal("no deployed SeedOnce seeder reported success")
}
