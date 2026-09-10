package bootstrap

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestNewConfigSource_File_KeepsTodaysWiring verifies that every topology keeps
// the optional file loader, polling watcher and control-only FileStore writer.
func TestNewConfigSource_File_KeepsTodaysWiring(t *testing.T) {
	for _, topology := range []deployinfra.Topology{
		deployinfra.TopologySingle, deployinfra.TopologyFilesystemReplicated, deployinfra.TopologyDynamoDBCoordinatedHA,
	} {
		for _, role := range []deployinfra.NodeRole{"", deployinfra.NodeRoleControl, deployinfra.NodeRoleWorker} {
			t.Run(string(topology)+"/"+string(role), func(t *testing.T) {
				cfg := testBootstrapConfig()
				cfg.ConfigFilePath = t.TempDir() + "/bridge.yaml"
				cfg.Topology, cfg.NodeRole, cfg.DevMode = topology, role, true
				cfg.PollInterval = "7s"
				app := NewApp(cfg)
				seed := defaultLogicalConfig(app.cfg)
				require.NoError(t, cfgparser.WriteFile(cfg.ConfigFilePath, seed))

				src, err := app.newConfigSource(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "file", src.layer.Name)
				assert.Equal(t, role != deployinfra.NodeRoleWorker, src.singleWriter)
				assert.Nil(t, app.dynamoDBClient, "file source must not construct or provision DynamoDB, even in dev mode")
				store, ok := src.store.(*cfgparser.FileStore)
				require.True(t, ok)
				assert.Equal(t, cfg.ConfigFilePath, store.Path)
				assert.Same(t, app.pluginRegistry, store.Registry)
				_, cas := src.store.(ports.ConditionalConfigStore)
				assert.False(t, cas)
				assert.IsType(t, &fileconfig.Source{}, src.layer.Loader)
				watcher, ok := src.layer.Watcher.(*fileconfig.Watcher)
				require.True(t, ok)
				fields := reflect.ValueOf(watcher).Elem()
				assert.Equal(t, int64(fileconfig.ModePoll), fields.FieldByName("mode").Int())
				assert.Equal(t, int64(7*time.Second), fields.FieldByName("pollInterval").Int())
				assert.False(t, fields.FieldByName("baselineHashSet").Bool(), "Observe owns the exact initial snapshot")
				loaded, err := src.layer.Loader.Load(t.Context())
				require.NoError(t, err)
				assert.Equal(t, seed.Bridge.ID, loaded.Bridge.ID)
				stored, err := src.store.Load(t.Context())
				require.NoError(t, err)
				assert.Equal(t, loaded, stored)
			})
		}
	}
}

// TestNewConfigSource_DynamoDB_WiresLoaderAsStore verifies one loader owns the
// read/watch/CAS state. No role asserts single-writer for this conditional store.
// Load(version 7) -> SaveIfVersion(7) -> version 8; another write at 7 -> conflict.
func TestNewConfigSource_DynamoDB_WiresLoaderAsStore(t *testing.T) {
	for _, role := range []deployinfra.NodeRole{"", deployinfra.NodeRoleControl, deployinfra.NodeRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			writes := 0
			client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
				var input struct {
					TableName                 string
					Key                       map[string]map[string]string
					ConsistentRead            bool
					ConditionExpression       string
					ExpressionAttributeValues map[string]map[string]string
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
				assert.Equal(t, "bridge-config-table", input.TableName)
				switch req.Header.Get("X-Amz-Target") {
				case "DynamoDB_20120810.GetItem":
					assert.Equal(t, "config#bridge-a", input.Key["PK"]["S"])
					assert.Equal(t, "current", input.Key["SK"]["S"])
					assert.True(t, input.ConsistentRead)
					return configJSONResponse(`{"Item":{"version":{"N":"7"},"data":{"S":"{\"bridge\":{\"id\":\"bridge-a\"},\"stores\":{\"lease\":{\"type\":\"memory\",\"options\":{\"acknowledge_single_replica\":true}}}}"}}}`, http.StatusOK), nil
				case "DynamoDB_20120810.PutItem":
					writes++
					assert.NotEmpty(t, input.ConditionExpression)
					assert.Equal(t, "7", input.ExpressionAttributeValues[":expected"]["N"])
					if writes == 1 {
						return configJSONResponse(`{}`, http.StatusOK), nil
					}
					return configJSONResponse(`{"__type":"ConditionalCheckFailedException","message":"stale version"}`, http.StatusBadRequest), nil
				default:
					t.Errorf("unexpected startup operation: %s", req.Header.Get("X-Amz-Target"))
					return nil, shared.ErrUnavailable
				}
			})
			cfg := dynamoDBSourceConfig()
			cfg.NodeRole = role
			app := NewApp(cfg, WithDynamoDBClient(client))
			src, err := app.newConfigSource(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "dynamodb", src.layer.Name)
			assert.False(t, src.singleWriter)
			assert.Same(t, client, app.dynamoDBClient)
			loader, ok := src.store.(*ddbconfig.Loader)
			require.True(t, ok)
			assert.Same(t, loader, src.layer.Watcher)
			assert.Same(t, loader, src.layer.Loader)
			cas, ok := src.store.(ports.ConditionalConfigStore)
			require.True(t, ok)
			loaded, err := src.layer.Loader.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 7, loaded.Version)
			require.NotNil(t, loaded.Stores.Lease.Config, "the injected registry must decode plugin options")
			require.NoError(t, cas.SaveIfVersion(t.Context(), loaded, 7))
			assert.Equal(t, 8, loaded.Version)
			require.ErrorIs(t, cas.SaveIfVersion(t.Context(), loaded, 7), shared.ErrVersionMismatch)
			assert.Equal(t, 8, loaded.Version)
			assert.Equal(t, 2, writes)
		})
	}
}

// TestNewConfigSource_DynamoDB_WatchOptions verifies bootstrap options reach the
// adapter without replacing its logger, clock, registry or watch state.
func TestNewConfigSource_DynamoDB_WatchOptions(t *testing.T) {
	for _, tc := range []struct {
		name, mode, poll, stream string
		wantMode                 ddbconfig.WatchMode
		wantPoll, wantStream     time.Duration
	}{
		{"defaults", "", "", "", ddbconfig.ModePoll, 30 * time.Second, 500 * time.Millisecond},
		{"poll override", "poll", "9s", "", ddbconfig.ModePoll, 9 * time.Second, 500 * time.Millisecond},
		{"invalid poll fallback", "poll", "invalid", "", ddbconfig.ModePoll, 30 * time.Second, 500 * time.Millisecond},
		{"streams", "streams", "11s", "250ms", ddbconfig.ModeStreams, 11 * time.Second, 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dynamoDBSourceConfig()
			cfg.PollInterval, cfg.ConfigDynamoDB.WatchMode, cfg.ConfigDynamoDB.StreamPollInterval = tc.poll, tc.mode, tc.stream
			client := configDynamoDBClient(t, func(*http.Request) (*http.Response, error) {
				t.Error("production source construction must perform no remote calls")
				return nil, shared.ErrUnavailable
			})
			app := NewApp(cfg, WithDynamoDBClient(client))
			app.clk = clocktest.NewAt(time.Unix(0, 0))
			src, err := app.newConfigSource(t.Context())
			require.NoError(t, err)
			fields := reflect.ValueOf(src.store).Elem()
			assert.Equal(t, int64(tc.wantMode), fields.FieldByName("mode").Int())
			assert.Equal(t, int64(tc.wantPoll), fields.FieldByName("pollInterval").Int())
			assert.Equal(t, int64(tc.wantStream), fields.FieldByName("streamPollInterval").Int())
			assert.Equal(t, reflect.ValueOf(app.clk).Pointer(), fields.FieldByName("clk").Elem().Pointer())
			assert.Equal(t, reflect.ValueOf(app.logger).Pointer(), fields.FieldByName("logger").Pointer())
			assert.Equal(t, reflect.ValueOf(app.pluginRegistry).Pointer(), fields.FieldByName("registry").Pointer())
			assert.Equal(t, tc.wantMode != ddbconfig.ModeStreams, fields.FieldByName("session").Elem().FieldByName("streams").IsNil())
		})
	}
}

func dynamoDBSourceConfig() deployinfra.BootstrapConfig {
	return deployinfra.BootstrapConfig{
		BridgeID: "bridge-a", AdminAPIKeyParam: "/admin",
		ConfigSource:   deployinfra.ConfigSourceDynamoDB,
		ConfigDynamoDB: &deployinfra.ConfigDynamoDBSettings{TableName: "bridge-config-table"},
	}
}
