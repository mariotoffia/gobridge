package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestNewConfigSource_DynamoDB_EnsuresTableOnlyInDevMode verifies production
// construction is offline; dev mode creates and waits for the configured table.
func TestNewConfigSource_DynamoDB_EnsuresTableOnlyInDevMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		dev        bool
	}{
		{"production poll", "poll", false},
		{"production streams", "streams", false},
		{"dev poll", "poll", true},
		{"dev streams", "streams", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var operations []string
			client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
				op := req.Header.Get("X-Amz-Target")
				operations = append(operations, op)
				var input struct {
					TableName           string
					StreamSpecification *struct {
						StreamEnabled  bool
						StreamViewType string
					}
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
				assert.Equal(t, "bridge-config-table", input.TableName)
				if op == "DynamoDB_20120810.CreateTable" {
					if tc.mode == "streams" {
						require.NotNil(t, input.StreamSpecification)
						assert.True(t, input.StreamSpecification.StreamEnabled)
						assert.Equal(t, "KEYS_ONLY", input.StreamSpecification.StreamViewType)
					} else {
						assert.Nil(t, input.StreamSpecification)
					}
				}
				return configJSONResponse(`{"Table":{"TableStatus":"ACTIVE"}}`, http.StatusOK), nil
			})
			cfg := dynamoDBSourceConfig()
			cfg.DevMode, cfg.ConfigDynamoDB.WatchMode = tc.dev, tc.mode
			app := NewApp(cfg, WithDynamoDBClient(client))
			_, err := app.newConfigSource(t.Context())
			require.NoError(t, err)
			if tc.dev {
				assert.Equal(t, []string{"DynamoDB_20120810.CreateTable", "DynamoDB_20120810.DescribeTable"}, operations)
			} else {
				assert.Empty(t, operations)
			}
		})
	}
}

// TestNewConfigSource_DynamoDB_EnsureFailurePropagates verifies a failed dev
// table preflight is not hidden by start-empty behavior.
func TestNewConfigSource_DynamoDB_EnsureFailurePropagates(t *testing.T) {
	client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "DynamoDB_20120810.CreateTable", req.Header.Get("X-Amz-Target"))
		return nil, shared.ErrNotAuthorized
	})
	cfg := dynamoDBSourceConfig()
	cfg.DevMode = true
	app := NewApp(cfg, WithDynamoDBClient(client))
	_, err := app.newConfigSource(t.Context())
	require.ErrorIs(t, err, shared.ErrNotAuthorized)
}

// TestNewConfigSource_DynamoDB_LazilyBuildsSharedClient verifies source wiring
// can initialize the same ambient client later used by the runtime store factory.
func TestNewConfigSource_DynamoDB_LazilyBuildsSharedClient(t *testing.T) {
	cfg := dynamoDBSourceConfig()
	cfg.AWSRegion = "eu-west-1"
	app := NewApp(cfg)
	src, err := app.newConfigSource(t.Context())
	require.NoError(t, err)
	require.NotNil(t, app.dynamoDBClient)
	assert.Equal(t, cfg.AWSRegion, app.dynamoDBClient.Options().Region)
	client := reflect.ValueOf(src.store).Elem().FieldByName("session").Elem().FieldByName("ddbConcrete")
	assert.Equal(t, reflect.ValueOf(app.dynamoDBClient).Pointer(), client.Pointer())
}

// TestNewConfigSource_DynamoDB_MissingItemStartsEmpty verifies the wrapper
// supplies a default only to the loader; the admin store retains missing-item truth.
func TestNewConfigSource_DynamoDB_MissingItemStartsEmpty(t *testing.T) {
	client := configDynamoDBClient(t, func(*http.Request) (*http.Response, error) {
		return configJSONResponse(`{}`, http.StatusOK), nil
	})
	app := NewApp(dynamoDBSourceConfig(), WithDynamoDBClient(client))
	src, err := app.newConfigSource(t.Context())
	require.NoError(t, err)
	got, err := src.layer.Loader.Load(t.Context())
	require.NoError(t, err)
	assert.Equal(t, defaultLogicalConfig(app.cfg), got)
	_, err = src.store.Load(t.Context())
	require.ErrorIs(t, err, shared.ErrNotFound)
}

// TestNewConfigSource_DynamoDB_StreamsUsesInjectedClientSettings verifies stream
// discovery uses the supplied endpoint and HTTP client, not ambient network access.
func TestNewConfigSource_DynamoDB_StreamsUsesInjectedClientSettings(t *testing.T) {
	discovered := make(chan string, 1)
	client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "config.invalid", req.URL.Host)
		switch req.Header.Get("X-Amz-Target") {
		case "DynamoDB_20120810.DescribeTable":
			return configJSONResponse(`{"Table":{"StreamSpecification":{"StreamEnabled":true},"LatestStreamArn":"test-stream"}}`, http.StatusOK), nil
		case "DynamoDBStreams_20120810.DescribeStream":
			discovered <- req.Header.Get("X-Amz-Target")
			return configJSONResponse(`{"StreamDescription":{"Shards":[]}}`, http.StatusOK), nil
		default:
			t.Errorf("unexpected operation: %s", req.Header.Get("X-Amz-Target"))
			return nil, shared.ErrUnavailable
		}
	})
	cfg := dynamoDBSourceConfig()
	cfg.ConfigDynamoDB.WatchMode = "streams"
	app := NewApp(cfg, WithDynamoDBClient(client))
	app.clk = clocktest.NewAt(time.Unix(0, 0))
	src, err := app.newConfigSource(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ch, err := src.layer.Watcher.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); wait.RequireClosed(t, ch, time.Second) })
	assert.Equal(t, "DynamoDBStreams_20120810.DescribeStream", wait.RequireReceive(t, discovered, time.Second))
}

// TestApp_DynamoDBSource_WiresManagerAndAdmin verifies Start uses the selected
// layer and CAS store while exposing the loaded config as the running state.
func TestApp_DynamoDBSource_WiresManagerAndAdmin(t *testing.T) {
	client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "DynamoDB_20120810.GetItem", req.Header.Get("X-Amz-Target"))
		return configJSONResponse(`{"Item":{"version":{"N":"7"},"data":{"S":"{\"bridge\":{\"id\":\"bridge-a\"}}"}}}`, http.StatusOK), nil
	})
	cfg := dynamoDBSourceConfig()
	cfg.AdminAddr, cfg.MonitorAddr, cfg.TransportHTTPAddr = ":0", ":0", ":0"
	app := NewApp(cfg, WithDynamoDBClient(client), WithCredentialStore(&fakePullStore{}),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
	app.clk = clocktest.NewAt(time.Unix(0, 0))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, app.Stop(ctx))
	})
	require.NoError(t, app.Start(t.Context()))
	require.NotNil(t, app.CurrentRuntime())
	assert.Equal(t, 7, app.CurrentAppliedConfig().Version)
	assert.Same(t, app.CurrentLogicalConfig(), app.CurrentAppliedConfig())
	api := reflect.ValueOf(app.httpServer).Elem().FieldByName("cfg")
	assert.False(t, api.FieldByName("ConfigSingleWriter").Bool())
	store := api.FieldByName("ConfigStore").Elem()
	assert.Equal(t, reflect.TypeOf((*ddbconfig.Loader)(nil)), store.Type())
	base := reflect.ValueOf(app.manager).Elem().FieldByName("base")
	assert.Equal(t, "dynamodb", base.FieldByName("Name").String())
	assert.Equal(t, store.Pointer(), base.FieldByName("Watcher").Elem().Pointer())
}
