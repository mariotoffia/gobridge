package bootstrap

import (
	"context"
	"testing"
	"time"

	infra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestExpiredApplyCannotLeaveHealthyNilRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	app := NewApp(infra.BootstrapConfig{BridgeID: "recovery-timeout", AdminAPIKeyParam: "/admin"},
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
	app.rootCtx = ctx
	original := &ports.BridgeConfig{Version: 1, Bridge: ports.BridgeSettings{ID: "recovery-timeout"}}
	t.Cleanup(func() {
		cancel()
		app.watchWg.Wait()
		_ = stopRuntime(context.Background(), app.CurrentRuntime(), app.CurrentAppliedConfig())
	})
	_, err := app.applyLogicalIfChanged(ctx, original, true)
	require.NoError(t, err)
	require.NotEmpty(t, app.lastAppliedFingerprint)
	// The exclusive swap has stopped its old runtime before its apply budget expires.
	require.NoError(t, stopRuntime(ctx, app.CurrentRuntime(), original))
	app.runtimeRef.Set(nil)
	expired, finish := context.WithDeadline(ctx, time.Time{})
	defer finish()
	require.ErrorIs(t, expired.Err(), context.DeadlineExceeded)
	app.recoverPrevious(expired, original)
	require.False(t, app.missing.Load())
	require.NoError(t, ctx.Err(), "the process still authorizes configuration")
	skipped, err := app.applyLogicalIfChanged(ctx, original, true)
	if app.runtimeTerminal() {
		require.False(t, skipped, "terminal recovery must not acknowledge the cached fingerprint")
		require.Error(t, err)
		require.Empty(t, app.lastAppliedFingerprint)
		return
	}
	require.NoError(t, err)
	require.NotNil(t, app.CurrentRuntime(), "an expired apply may recover or terminate, never leave a healthy nil runtime")
	require.True(t, app.CurrentRuntime().IsRunning())
}
