package runtime_test

import (
	"context"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP-23: DLQ Router Close/Route Race
//
// Close() calls close(r.buffer) while Route() may concurrently send on
// the channel, causing a panic. The fix uses a mutex + stopped flag.
// ═══════════════════════════════════════════════════════════════════════════

// TestDLQRouter_ConcurrentCloseAndRoute launches goroutines calling Route
// in a tight loop while Close() is called concurrently. No panic must occur.
func TestDLQRouter_ConcurrentCloseAndRoute(t *testing.T) {
	store := NewFakeDLQStore()
	router := runtime.NewDLQRouterFromConfig(runtime.DLQRouterConfig{
		Store:      store,
		BufferSize: 10,
		EnqTimeout: 50 * time.Millisecond,
		Workers:    2,
	})

	ctx := context.Background()
	router.Start(ctx)

	env := &messaging.Envelope{
		ID:      "race-test",
		Subject: "test/dlq-race",
		Payload: []byte("payload"),
	}

	var wg sync.WaitGroup
	const routeGoroutines = 10
	const routeIterations = 100

	// Launch goroutines calling Route concurrently.
	for range routeGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range routeIterations {
				_ = router.Route(
					ctx, env,
					"route-1", "bind-1", "sess-1", "src-1",
					shared.ErrUnavailable, 1,
				)
			}
		}()
	}

	// Yield so Route goroutines get scheduled before Close.
	goruntime.Gosched()
	router.Close()

	// All Route goroutines must finish without panic.
	wg.Wait()
}

// TestDLQRouter_DoubleClose verifies that calling Close twice is safe.
func TestDLQRouter_DoubleClose(t *testing.T) {
	store := NewFakeDLQStore()
	router := runtime.NewDLQRouterFromConfig(runtime.DLQRouterConfig{
		Store:      store,
		BufferSize: 10,
		Workers:    1,
	})

	ctx := context.Background()
	router.Start(ctx)

	// Close twice — second should be a no-op.
	router.Close()
	router.Close()
}

// TestDLQRouter_RouteAfterClose falls back to synchronous write.
func TestDLQRouter_RouteAfterClose(t *testing.T) {
	store := NewFakeDLQStore()
	router := runtime.NewDLQRouterFromConfig(runtime.DLQRouterConfig{
		Store:      store,
		BufferSize: 10,
		Workers:    1,
	})

	ctx := context.Background()
	router.Start(ctx)
	router.Close()

	// Route after close should fall back to writeDirect (no panic).
	env := &messaging.Envelope{
		ID:      "after-close",
		Subject: "test/after-close",
		Payload: []byte("payload"),
	}

	err := router.Route(
		ctx, env,
		"route-1", "bind-1", "sess-1", "src-1",
		shared.ErrUnavailable, 1,
	)
	assert.NoError(t, err, "Route after Close should fall back to direct write")
	assert.Equal(t, 1, store.Count(), "entry should be written synchronously")
}
