package runtime_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type bestEffortLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *bestEffortLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *bestEffortLogBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []string
	for _, line := range strings.Split(b.b.String(), "\n") {
		if strings.Contains(line, "direct_hold subscription is best-effort") {
			result = append(result, line)
		}
	}
	return result
}

func TestBestEffortActivation_OncePerSuccessfulStart(t *testing.T) {
	logs := &bestEffortLogBuffer{}
	for generation := range 2 {
		rt := runtime.New(runtime.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		rx := NewFakeReceiver()
		cfg := runtime.RouteConfig{
			ID: "readings", SourceReceiverID: "mqtt-in",
			SourceBestEffortTopics: []string{"readings/#", "temperatures/#"},
			Policy: routing.RoutePolicy{
				DeliveryMode: routing.DeliveryDirectHold, AllowRetryDrop: true,
				OnPermanentFailure: routing.FailureDrop, OnExpired: routing.ExpiredDrop,
			},
		}
		require.NoError(t, rt.AddRoute(cfg, rx, NewFakeSender(), nil, nil))
		t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
		require.NoError(t, rt.ValidateRoutes())
		require.NoError(t, rt.ValidateRoutes())
		assert.Len(t, logs.lines(), generation*2)
		require.NoError(t, rt.Start(t.Context()))
		require.Error(t, rt.Start(t.Context()))
		del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "reading"}))
		require.NoError(t, rx.Emit(t.Context(), del))
		wait.Until(t, time.Second, "delivery settled", del.IsAcked)
		require.NoError(t, rt.Stop(context.Background()))
		lines := logs.lines()
		require.Len(t, lines, (generation+1)*2)
		for _, line := range lines {
			assert.Contains(t, line, `"level":"INFO"`)
			assert.Contains(t, line, `"route_id":"readings"`)
			assert.Contains(t, line, `"receiver_id":"mqtt-in"`)
		}
		assert.Contains(t, lines[generation*2], `"topic":"readings/#"`)
		assert.Contains(t, lines[generation*2+1], `"topic":"temperatures/#"`)
	}
}

func TestBestEffortActivation_FailedStartIsSilent(t *testing.T) {
	logs := &bestEffortLogBuffer{}
	rt := runtime.New(runtime.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceBestEffortTopics = []string{"readings/#"}
	cfg.Policy.DispatchMode = routing.DispatchFanOut
	require.NoError(t, rt.AddRoute(cfg, rx, tx, sess, sessCfg))
	require.Error(t, rt.Start(t.Context()))
	assert.Empty(t, logs.lines())
	require.NoError(t, rt.Stop(context.Background()))
}
