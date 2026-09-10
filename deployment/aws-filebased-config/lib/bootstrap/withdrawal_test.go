package bootstrap

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	infra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withdrawalConfig(version int) *ports.BridgeConfig {
	return &ports.BridgeConfig{Version: version, Bridge: ports.BridgeSettings{ID: "withdrawal", LogLevel: "debug"}}
}

func TestMissingFencesWhilePresentApplyIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	source := &streamingConfigObserver{changes: make(chan ports.ConfigObservation, 3)}
	app := NewApp(infra.BootstrapConfig{BridgeID: "withdrawal", AdminAPIKeyParam: "/admin", PollInterval: "1h"},
		WithParameterResolver(parameterResolverFunc(func(ctx context.Context, _ string) (string, error) {
			select {
			case entered <- ctx:
			default:
			}
			<-release
			return "admin-secret-key-123456", nil
		})))
	app.manager = config.NewManager(config.Layer{Name: "target", Loader: source})
	app.rootCtx = ctx
	old := goruntime.New()
	require.NoError(t, old.Start(ctx))
	app.runtimeRef.Set(old)
	app.appliedRef.Set(withdrawalConfig(0))
	app.activated.Store(true)
	t.Cleanup(func() {
		unblock()
		cancel()
		app.watchWg.Wait()
		app.manager.Stop()
		_ = old.Stop(context.Background())
	})
	app.watchWg.Go(func() { app.observeRepository(ctx, nil) })
	source.changes <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: withdrawalConfig(1)}
	wait.RequireReceive(t, entered, time.Second)
	source.changes <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: withdrawalConfig(2)}
	source.changes <- ports.ConfigObservation{Kind: ports.ConfigMissing}
	wait.Until(t, 100*time.Millisecond, "missing bypasses queued present while apply is blocked", func() bool {
		return app.missing.Load() && !old.IsRunning()
	})
}

func TestWithdrawalCancelsRecoveryBeforeItCanStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	source := &streamingConfigObserver{changes: make(chan ports.ConfigObservation, 1)}
	starts := &runtimeStartLog{started: make(chan struct{}, 1)}
	app := NewApp(infra.BootstrapConfig{BridgeID: "withdrawal", AdminAPIKeyParam: "/admin", PollInterval: "1h"},
		WithLogger(slog.New(slog.NewJSONHandler(starts, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithParameterResolver(parameterResolverFunc(func(ctx context.Context, _ string) (string, error) {
			entered <- ctx
			<-release
			return "admin-secret-key-123456", nil
		})))
	app.manager = config.NewManager(config.Layer{Name: "target", Loader: source})
	app.rootCtx = ctx
	finished := make(chan struct{})
	t.Cleanup(func() {
		unblock()
		cancel()
		app.watchWg.Wait()
		app.manager.Stop()
		_ = stopRuntime(context.Background(), app.CurrentRuntime(), app.CurrentAppliedConfig())
	})
	app.watchWg.Go(func() { app.observeRepository(ctx, nil) })
	app.watchWg.Go(func() {
		app.mu.Lock()
		defer app.mu.Unlock()
		defer close(finished)
		app.recoverPrevious(ctx, withdrawalConfig(1))
	})
	recoveryCtx := wait.RequireReceive(t, entered, time.Second)
	source.changes <- ports.ConfigObservation{Kind: ports.ConfigMissing}
	wait.Until(t, time.Second, "withdrawal observed", app.missing.Load)
	assert.True(t, wait.Poll(100*time.Millisecond, func() bool { return recoveryCtx.Err() != nil }), "withdrawal must cancel the recovery generation")
	unblock()
	wait.RequireClosed(t, finished, time.Second)
	require.Empty(t, starts.started, "a withdrawn recovery must never reach Runtime.Start")
}

func TestWithdrawalFencesRecoveryBeforePublication(t *testing.T) {
	app := NewApp(infra.BootstrapConfig{BridgeID: "unpublished"})
	ctx, finish := app.recoveryContext(t.Context())
	defer finish()
	rt := goruntime.New()
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	plan := &runtimePlan{epoch: app.observationEpoch.Load(), runtime: rt}
	require.NoError(t, app.startRecovery(ctx, t.Context(), plan))
	require.Nil(t, app.CurrentRuntime(), "candidate has not been published yet")
	app.withdrawConfiguration()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.False(t, rt.IsRunning(), "withdrawal must also fence the unpublished candidate")
	require.Error(t, app.startRecovery(ctx, t.Context(), plan))
}
