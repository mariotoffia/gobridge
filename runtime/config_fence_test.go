package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/require"
)

func TestConfigFencePreventsRestart(t *testing.T) {
	rt := goruntime.New()
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	rt.Fence()
	require.False(t, rt.IsRunning())
	require.Error(t, rt.Start(context.Background()))
	require.NoError(t, rt.Stop(context.Background()))
	rt.Fence()
	require.NoError(t, rt.Stop(context.Background()))
}

func TestFenceRejectsEveryInjectionEntryPoint(t *testing.T) {
	rt := goruntime.New()
	cfg, receiver, sender := helperQuiescentRoute("inject-fence", nil)
	require.NoError(t, rt.AddRoute(cfg, receiver, sender, nil, nil))
	require.NoError(t, rt.Start(t.Context()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "before-fence", Subject: "test"})
	require.NoError(t, rt.Inject(t.Context(), cfg.ID, env))
	require.Equal(t, 1, sender.SentCount())
	rt.Fence()
	for _, inject := range []func(context.Context) error{
		func(ctx context.Context) error { return rt.Inject(ctx, cfg.ID, env) },
		func(ctx context.Context) error { return rt.InjectToBinding(ctx, cfg.ID, "", env) },
		func(ctx context.Context) error { return rt.InjectRedrive(ctx, cfg.ID, "", env) },
	} {
		require.ErrorContains(t, inject(context.Background()), "not running")
	}
	require.Equal(t, 1, sender.SentCount(), "withdrawal must fence admin injection and redrive too")
}
