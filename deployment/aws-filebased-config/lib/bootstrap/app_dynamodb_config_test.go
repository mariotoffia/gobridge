package bootstrap

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// An external writer must reach the running App through the adapter watcher and
// config manager, without an admin commit applying the change in-band.
func TestIntegration_AppReloadsDynamoDBConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Cleanup(ddblocal.Shutdown)
	client := ddblocal.Client(t)

	for _, tc := range []struct {
		name, mode string
		empty      bool
	}{
		{"poll_seeded", "poll", false},
		{"streams_seeded", "streams", false},
		{"poll_empty", "poll", true},
		{"streams_empty", "streams", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := ddblocal.UniqueTable("app-config")
			ddblocal.CleanupTable(t, client, table)

			// Observe real Streams requests without replacing their responses.
			// This prevents a fallback to table polling from passing the Streams cases.
			var streamReads atomic.Int64
			opts := client.Options()
			httpClient := opts.HTTPClient
			opts.HTTPClient = configHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := httpClient.Do(req)
				if err == nil && resp.StatusCode == http.StatusOK &&
					req.Header.Get("X-Amz-Target") == "DynamoDBStreams_20120810.GetRecords" {
					streamReads.Add(1)
				}
				return resp, err
			})

			cfg := dynamoDBSourceConfig()
			cfg.ConfigDynamoDB.TableName = table
			cfg.ConfigDynamoDB.WatchMode = tc.mode
			cfg.ConfigDynamoDB.StreamPollInterval = "1s"
			cfg.PollInterval = "1s"
			if tc.mode == "streams" {
				cfg.PollInterval = "1h" // no fallback poll can fire during this test
			}
			cfg.DevMode = tc.empty // empty startup also exercises App-owned table creation
			cfg.AdminAddr, cfg.MonitorAddr, cfg.TransportHTTPAddr = ":0", ":0", ":0"
			app := NewApp(cfg,
				WithDynamoDBClient(dynamodb.New(opts)),
				WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
				WithCredentialStore(&fakePullStore{}),
			)
			fc := clocktest.NewAt(time.Unix(0, 0))
			app.clk = fc
			var installs atomic.Int64
			app.onRuntimeInstalled = func() { installs.Add(1) }
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, app.Stop(ctx))
			})

			mode := ddbconfig.ModePoll
			if tc.mode == "streams" {
				mode = ddbconfig.ModeStreams
			}
			writer := ddbconfig.NewLoader(client,
				ddbconfig.WithTableName(table),
				ddbconfig.WithBridgeID(cfg.BridgeID),
				ddbconfig.WithRegistry(app.pluginRegistry),
				ddbconfig.WithWatchMode(mode),
			)
			initialVersion, initialLevel := 0, ""
			if !tc.empty {
				require.NoError(t, writer.EnsureTable(t.Context()))
				seed := &ports.BridgeConfig{Bridge: ports.BridgeSettings{
					ID: cfg.BridgeID, DeploymentMode: "standalone", LogLevel: "info",
				}}
				require.NoError(t, writer.Save(t.Context(), seed))
				initialVersion, initialLevel = 1, "info"
			}

			require.NoError(t, app.Start(t.Context()))
			initialRuntime := app.CurrentRuntime()
			require.NotNil(t, initialRuntime)
			initialConfig := app.CurrentAppliedConfig()
			require.NotNil(t, initialConfig)
			require.Equal(t, initialVersion, initialConfig.Version)
			require.Equal(t, initialLevel, initialConfig.Bridge.LogLevel)
			require.Equal(t, int64(1), installs.Load())
			if tc.empty {
				_, err := writer.Load(t.Context())
				require.ErrorIs(t, err, shared.ErrNotFound, "start-empty must not persist the fallback")
			}

			wait.Until(t, 5*time.Second, "DynamoDB watcher cadence is ready", func() bool {
				if tc.mode == "streams" {
					return streamReads.Load() > 0 && fc.TimerCount() > 0
				}
				return slices.Contains(fc.TickerPeriods(), time.Second)
			})
			readsBeforeSave := streamReads.Load()

			// Save mutates Version: use a fresh object, never the shared logical/
			// applied pointer. Only a reload may change what the App reports.
			changed := &ports.BridgeConfig{Bridge: ports.BridgeSettings{
				ID: cfg.BridgeID, DeploymentMode: "standalone", LogLevel: "debug",
			}}
			require.NoError(t, writer.Save(t.Context(), changed))
			require.Equal(t, initialVersion+1, changed.Version)
			source, err := writer.Load(t.Context())
			require.NoError(t, err)
			require.Equal(t, changed.Version, source.Version)
			require.Equal(t, changed.Bridge, source.Bridge)
			require.Same(t, initialRuntime, app.CurrentRuntime(), "no watch tick has fired")
			require.Equal(t, initialVersion, initialConfig.Version)
			require.Equal(t, initialLevel, initialConfig.Bridge.LogLevel)

			fc.Advance(time.Second)
			streamTicks := 1
			wait.Until(t, 5*time.Second, "runtime replacement and config health converge", func() bool {
				health := app.configWatchHealth()
				applied := app.CurrentAppliedConfig()
				logical := app.CurrentLogicalConfig()
				// installPlan's callback precedes NotifyApplyResult. Wait for the
				// full acknowledgement, not just the new runtime or version.
				if app.CurrentRuntime() != nil && app.CurrentRuntime() != initialRuntime &&
					installs.Load() == 2 && applied != nil && logical != nil &&
					applied.Version == source.Version && logical.Version == source.Version &&
					health.DesiredVersion != nil && *health.DesiredVersion == source.Version &&
					health.RunningVersion != nil && *health.RunningVersion == source.Version &&
					!health.ReconfigurePending && !health.Degraded && health.LastApplyError == "" {
					return true
				}
				// Streams may need another GetRecords call before the write is
				// visible. Advance only an observed timer, well short of the
				// one-hour fallback poll interval.
				if tc.mode == "streams" && streamTicks < 10 && fc.TimerCount() > 0 {
					fc.Advance(time.Second)
					streamTicks++
				}
				return false
			})
			assert.Equal(t, source, app.CurrentLogicalConfig())
			assert.Equal(t, source, app.CurrentAppliedConfig())
			assert.Empty(t, app.configWatchHealth().Reason)
			if tc.mode == "streams" {
				assert.Greater(t, streamReads.Load(), readsBeforeSave)
				assert.NotContains(t, fc.TickerPeriods(), time.Hour, "Streams must not fall back to polling")
			}
		})
	}
}
