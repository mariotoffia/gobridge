package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// slowFlushExporter models the OTEL-N5 scenario: a metrics exporter whose Flush
// is slow-but-successful (it consumes the whole flush budget, then returns nil)
// and whose Close (provider.Shutdown) reports whatever ctx it is handed. It
// records the ctx error observed at Close entry so a test can prove Close was
// given a LIVE budget rather than Flush's already-exhausted one.
type slowFlushExporter struct {
	ports.NoopExporter
	mu          sync.Mutex
	flushCalled bool
	closeCalled bool
	closeCtxErr error
}

func (e *slowFlushExporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	e.flushCalled = true
	e.mu.Unlock()
	// Consume the ENTIRE flush budget: block until the flush ctx deadline
	// fires, then report success (a slow flush, not a failing one).
	<-ctx.Done()
	return nil
}

func (e *slowFlushExporter) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closeCalled = true
	e.closeCtxErr = ctx.Err()
	e.mu.Unlock()
	// Return the ctx error the way an OTel provider Shutdown would when its
	// deadline has already passed. With a live budget this is nil.
	return ctx.Err()
}

// TestStop_MetricsCloseGetsOwnBudget_NoSpuriousError is the regression guard for
// OTEL-N5: metrics Close must get its OWN shutdown budget, not share the halved
// flush budget with a slow Flush. A slow-but-successful Flush that consumes the
// flush budget must NOT push Close to an already-expired deadline and make an
// otherwise-clean provider shutdown append a spurious ctx error to Stop's
// result.
func TestStop_MetricsCloseGetsOwnBudget_NoSpuriousError(t *testing.T) {
	exp := &slowFlushExporter{}

	// shutdownTimeout 200ms => flush budget 100ms, close budget 200ms. Flush
	// blocks for the full 100ms flush budget; before the fix Close then inherited
	// that exhausted ctx. The close budget (200ms) is created before Flush yet
	// still leaves ~100ms of slack when Close runs — ample for a loaded CI box.
	rt := goruntime.New(
		goruntime.WithInstanceID("otel-n5"),
		goruntime.WithMetrics(exp),
		goruntime.WithShutdownTimeout(200*time.Millisecond),
	)

	require.NoError(t, rt.Start(context.Background()))

	// Caller ctx is never cancelled: any error on Stop's result would be the
	// spurious Close deadline error this fix removes.
	err := rt.Stop(context.Background())
	require.NoError(t, err, "clean shutdown must not append a spurious metrics Close error")

	exp.mu.Lock()
	defer exp.mu.Unlock()
	assert.True(t, exp.flushCalled, "Flush must be invoked during Stop")
	assert.True(t, exp.closeCalled, "Close must be invoked during Stop so the provider shuts down")
	assert.NoError(t, exp.closeCtxErr,
		"Close must receive its own live budget, not Flush's exhausted flush ctx")
}
