package runtime_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// syncBuffer is a goroutine-safe bytes.Buffer for capturing slog output
// (Start's session goroutines may log concurrently with the test's read).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStart_SharedSessionDrainer_WarnsOnConfigBleed covers audit R8 (LOW):
// exactly one outbox drainer exists per session partition, built from the
// FIRST shared_outbox route that references the session. A second route
// sharing that session drains its records under the first route's policy,
// sender and RouteID — a silent config bleed. Start must surface it at Warn
// naming both routes.
func TestStart_SharedSessionDrainer_WarnsOnConfigBleed(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	var logBuf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rt := newTestRuntime("bridge-drainer-bleed", outbox, lease, nil,
		goruntime.WithLogger(logger))

	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-shared-sess")

	for _, id := range []string{"route-first", "route-second"} {
		cfg := goruntime.RouteConfig{
			ID:     id,
			Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: []routing.DestinationBinding{
				{ID: id + "-binding", Address: "devices/" + id},
			},
		}
		sc := sessCfg
		if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)

	out := logBuf.String()
	if !strings.Contains(out, "config bleed") {
		t.Fatalf("R8: expected a config-bleed warning for the shared-session drainer, log output:\n%s", out)
	}
	if !strings.Contains(out, "route-first") || !strings.Contains(out, "route-second") {
		t.Fatalf("R8: warning must name both the drainer-owning and the bleeding route, log output:\n%s", out)
	}
}
